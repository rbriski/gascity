package graphstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore/canon"
)

// Append commits events to streamID atomically in one BEGIN IMMEDIATE
// transaction (I-1, I-13). The protocol, in order:
//
//   - reject any unregistered (engine, type) with ErrUnknownEventType (I-5);
//   - dedupe events by (stream_id, idem_token): tokens already present are
//     reported in AppendResult.Duplicates and appended nothing (R-IDEM);
//   - if every event is a duplicate, acknowledge the replay and return without
//     enforcing expectedVersion (a crash-replay finds its tokens after the head
//     has already advanced);
//   - otherwise require head == expectedVersion, else ErrWrongExpectedVersion —
//     the loud CAS that kills S0.4;
//   - reject leaseEpoch below the stream's current writer-lease epoch with
//     ErrLeaseFenced (belt on top of expectedVersion);
//   - assign dense seq = expectedVersion+1.., compute payload_hash and chain_hash
//     inside the transaction (D-SEC-1), and INSERT;
//   - COMMIT (one fsync under synchronous=FULL).
func (s *Store) Append(ctx context.Context, streamID, engine string, expectedVersion, leaseEpoch uint64, events []JournalEvent) (AppendResult, error) {
	if streamID == "" {
		return AppendResult{}, fmt.Errorf("graphstore: append: empty stream id")
	}
	if len(events) == 0 {
		return AppendResult{}, fmt.Errorf("graphstore: append: no events")
	}
	for i, e := range events {
		if !s.isRegistered(engine, e.Type) {
			return AppendResult{}, fmt.Errorf("graphstore: append: event %d (%s, %s): %w", i, engine, e.Type, ErrUnknownEventType)
		}
		if e.Payload == nil {
			return AppendResult{}, fmt.Errorf("graphstore: append: event %d has nil payload", i)
		}
	}

	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return AppendResult{}, fmt.Errorf("graphstore: append: begin: %w", mapSQLiteBusy(err))
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	result := AppendResult{Duplicates: map[int]uint64{}}
	fresh := make([]int, 0, len(events))
	for i, e := range events {
		if e.IdemToken == "" {
			fresh = append(fresh, i)
			continue
		}
		existing, ok, err := lookupIdem(ctx, tx, streamID, e.IdemToken)
		if err != nil {
			return AppendResult{}, err
		}
		if ok {
			// R-IDEM: a true replay carries a byte-identical R-CANON payload
			// (and identical framing). If the token is bound to a row whose
			// payload or framing differs, this is not an honest replay —
			// discarding it would lose data, so fail loudly instead.
			if canon.Hash(e.Payload) != existing.payloadHash ||
				e.Type != existing.typ ||
				e.Substream != existing.substream ||
				e.IRContractVersion != existing.irVersion {
				return AppendResult{}, fmt.Errorf(
					"graphstore: append: idem token %q on stream %q already bound to seq %d: %w",
					e.IdemToken, streamID, existing.seq, ErrIdemTokenReuse)
			}
			result.Duplicates[i] = existing.seq
			continue
		}
		fresh = append(fresh, i)
	}

	// Pure idempotent replay: nothing new to write. Acknowledge with the first
	// input event's existing seq and do not enforce expectedVersion.
	if len(fresh) == 0 {
		if err := tx.Commit(); err != nil {
			return AppendResult{}, fmt.Errorf("graphstore: append: commit (replay): %w", mapSQLiteBusy(err))
		}
		result.FirstSeq = result.Duplicates[0]
		return result, nil
	}

	head, prevChain, err := headAndChain(ctx, tx, streamID)
	if err != nil {
		return AppendResult{}, err
	}
	if head != expectedVersion {
		return AppendResult{}, fmt.Errorf("graphstore: append: stream %q head %d, expected %d: %w", streamID, head, expectedVersion, ErrWrongExpectedVersion)
	}
	if err := checkLeaseEpoch(ctx, tx, streamID, leaseEpoch); err != nil {
		return AppendResult{}, err
	}

	prev := prevChain
	if head == 0 {
		prev = genesisHash(streamID, s.cityID)
	}
	appendedAt := time.Now().UTC().Format(time.RFC3339Nano)
	seq := head
	for _, idx := range fresh {
		e := events[idx]
		seq++
		payloadHash := canon.Hash(e.Payload)
		chain := chainHash(prev, streamID, seq, engine, e.Type, e.Substream, e.IRContractVersion, payloadHash)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO journal
			   (stream_id, seq, substream, engine, type, ir_contract_version,
			    idem_token, payload, payload_hash, chain_hash, lease_epoch, appended_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			streamID, seq, e.Substream, engine, e.Type, e.IRContractVersion,
			nullableToken(e.IdemToken), e.Payload, payloadHash[:], chain[:], leaseEpoch, appendedAt,
		); err != nil {
			return AppendResult{}, fmt.Errorf("graphstore: append: insert seq %d: %w", seq, mapSQLiteBusy(err))
		}
		if idx == fresh[0] {
			result.FirstSeq = seq
		}
		prev = chain
	}

	if err := tx.Commit(); err != nil {
		return AppendResult{}, fmt.Errorf("graphstore: append: commit: %w", mapSQLiteBusy(err))
	}
	return result, nil
}

// ReadStream returns committed events for streamID with fromSeq <= seq <= toSeq
// ordered by seq. toSeq == 0 means up to the current head.
func (s *Store) ReadStream(ctx context.Context, streamID string, fromSeq, toSeq uint64) ([]StoredEvent, error) {
	if toSeq == 0 {
		toSeq = ^uint64(0) >> 1 // max signed int64; seqs never approach this
	}
	rows, err := s.readDB.QueryContext(ctx,
		`SELECT seq, substream, engine, type, ir_contract_version, idem_token,
		        payload, payload_hash, chain_hash, lease_epoch
		   FROM journal
		  WHERE stream_id = ? AND seq >= ? AND seq <= ?
		  ORDER BY seq`,
		streamID, fromSeq, toSeq,
	)
	if err != nil {
		return nil, fmt.Errorf("graphstore: read stream %q: %w", streamID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []StoredEvent
	for rows.Next() {
		var (
			ev        StoredEvent
			token     sql.NullString
			payHash   []byte
			chainHash []byte
		)
		ev.StreamID = streamID
		if err := rows.Scan(
			&ev.Seq, &ev.Substream, &ev.Engine, &ev.Type, &ev.IRContractVersion,
			&token, &ev.Payload, &payHash, &chainHash, &ev.LeaseEpoch,
		); err != nil {
			return nil, fmt.Errorf("graphstore: read stream %q: scan: %w", streamID, err)
		}
		if token.Valid {
			ev.IdemToken = token.String
		}
		copy(ev.PayloadHash[:], payHash)
		copy(ev.ChainHash[:], chainHash)
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("graphstore: read stream %q: %w", streamID, err)
	}
	return out, nil
}

// Head returns the current head (MAX(seq)) for streamID; 0 means absent.
func (s *Store) Head(ctx context.Context, streamID string) (uint64, error) {
	var head uint64
	if err := s.readDB.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM journal WHERE stream_id = ?`, streamID,
	).Scan(&head); err != nil {
		return 0, fmt.Errorf("graphstore: head %q: %w", streamID, err)
	}
	return head, nil
}

// Verify walks streamID from its first surviving seq and recomputes every
// payload_hash and chain_hash, returning ErrChainBroken (wrapped with the
// offending seq) on the first mismatch. This backs `gc journal verify`.
//
// An untruncated stream starts at seq 1 anchored on the genesis hash. A
// retention-truncated stream starts at anchorSeq+1: its chain resumes from the
// cut_chain_hash the covering snapshot recorded at TruncateBelowAnchor, so the
// hash chain stays verifiable across the cut with the snapshot as the new anchor
// (SEC-T-6). A truncated head with no such cut anchor is journal tampering.
func (s *Store) Verify(ctx context.Context, streamID string) error {
	events, err := s.ReadStream(ctx, streamID, 1, 0)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	firstSeq := events[0].Seq
	var prev [32]byte
	if firstSeq == 1 {
		prev = genesisHash(streamID, s.cityID)
	} else {
		cut, ok, err := s.cutChainHashAt(ctx, streamID, firstSeq-1)
		if err != nil {
			return fmt.Errorf("graphstore: verify %q: reading cut anchor at %d: %w", streamID, firstSeq-1, err)
		}
		if !ok {
			return fmt.Errorf("graphstore: verify %q: stream truncated below seq %d with no snapshot cut anchor: %w", streamID, firstSeq, ErrChainBroken)
		}
		prev = cut
	}
	wantSeq := firstSeq
	for _, e := range events {
		if e.Seq != wantSeq {
			return fmt.Errorf("graphstore: verify %q: gap or reorder at seq %d (expected %d): %w", streamID, e.Seq, wantSeq, ErrChainBroken)
		}
		if got := canon.Hash(e.Payload); got != e.PayloadHash {
			return fmt.Errorf("graphstore: verify %q seq %d: payload hash mismatch: %w", streamID, e.Seq, ErrChainBroken)
		}
		want := chainHash(prev, streamID, e.Seq, e.Engine, e.Type, e.Substream, e.IRContractVersion, e.PayloadHash)
		if want != e.ChainHash {
			return fmt.Errorf("graphstore: verify %q seq %d: chain hash mismatch: %w", streamID, e.Seq, ErrChainBroken)
		}
		prev = e.ChainHash
		wantSeq++
	}
	return nil
}

// idemRow is the committed row an existing idem token is bound to. Append
// compares an incoming event against these fields to distinguish an honest
// byte-identical replay from a divergent reuse (B1 / R-IDEM).
type idemRow struct {
	seq         uint64
	payloadHash [32]byte
	typ         string
	substream   string
	irVersion   string
}

func lookupIdem(ctx context.Context, tx *sql.Tx, streamID, token string) (idemRow, bool, error) {
	var (
		row     idemRow
		payHash []byte
	)
	err := tx.QueryRowContext(ctx,
		`SELECT seq, payload_hash, type, substream, ir_contract_version
		   FROM journal WHERE stream_id = ? AND idem_token = ?`,
		streamID, token,
	).Scan(&row.seq, &payHash, &row.typ, &row.substream, &row.irVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return idemRow{}, false, nil
	}
	if err != nil {
		return idemRow{}, false, fmt.Errorf("graphstore: append: idem lookup %q/%q: %w", streamID, token, err)
	}
	copy(row.payloadHash[:], payHash)
	return row, true, nil
}

// headAndChain returns the current head seq and the chain_hash of that head row
// (zero-value hash when the stream is empty), read inside the append txn.
func headAndChain(ctx context.Context, tx *sql.Tx, streamID string) (uint64, [32]byte, error) {
	var (
		head  uint64
		chain []byte
	)
	err := tx.QueryRowContext(ctx,
		`SELECT seq, chain_hash FROM journal
		  WHERE stream_id = ? ORDER BY seq DESC LIMIT 1`,
		streamID,
	).Scan(&head, &chain)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, [32]byte{}, nil
	}
	if err != nil {
		return 0, [32]byte{}, fmt.Errorf("graphstore: append: reading head of %q: %w", streamID, err)
	}
	var out [32]byte
	copy(out[:], chain)
	return head, out, nil
}

// checkLeaseEpoch rejects a stale fencing epoch. When no lease row exists the
// stream is unfenced and the append proceeds (safety is expectedVersion).
func checkLeaseEpoch(ctx context.Context, tx *sql.Tx, streamID string, leaseEpoch uint64) error {
	var current uint64
	err := tx.QueryRowContext(ctx,
		`SELECT epoch FROM writer_lease WHERE stream_id = ?`, streamID,
	).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("graphstore: append: reading lease epoch of %q: %w", streamID, err)
	}
	if leaseEpoch < current {
		return fmt.Errorf("graphstore: append: lease epoch %d below current %d: %w", leaseEpoch, current, ErrLeaseFenced)
	}
	return nil
}

func nullableToken(token string) any {
	if token == "" {
		return nil
	}
	return token
}

// chainHash computes chain_hash per D-SEC-1, as amended by the DDL-freeze S6
// decision. The chained preimage is
//
//	SHA256(prev.chain_hash || stream_id || seq || engine || type ||
//	       substream || ir_contract_version || payload_hash)
//
// To remove the boundary ambiguity of raw concatenation over variable-length
// string fields, each variable-length field is length-prefixed (8-byte
// big-endian) while the two fixed 32-byte hashes and the 8-byte seq are written
// raw. This is the sole canonical framing.
//
// substream is folded into the chain immediately after type (S6, 01-architecture
// §7 decision "leaning yes", adopted): substream is semantics-bearing (channel
// routing), so a retention-gated delete+reinsert that changes only substream must
// alter chain_hash and be caught by Verify. Its fixed length-prefixed position is
// load-bearing — do not move it. The number format in canon.canonicalNumber (Go
// strconv 'g') remains pending DDL-freeze confirmation (§7 S7); do not change that
// framing without that decision.
func chainHash(prev [32]byte, streamID string, seq uint64, engine, typ, substream, irVersion string, payloadHash [32]byte) [32]byte {
	h := sha256.New()
	h.Write(prev[:])
	writeLenPrefixed(h, []byte(streamID))
	var seqBuf [8]byte
	binary.BigEndian.PutUint64(seqBuf[:], seq)
	h.Write(seqBuf[:])
	writeLenPrefixed(h, []byte(engine))
	writeLenPrefixed(h, []byte(typ))
	writeLenPrefixed(h, []byte(substream))
	writeLenPrefixed(h, []byte(irVersion))
	h.Write(payloadHash[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// genesisHash is the seq=1 chain anchor: SHA256(stream_id || city_id), framed
// with the same length prefixing as chainHash.
func genesisHash(streamID, cityID string) [32]byte {
	h := sha256.New()
	writeLenPrefixed(h, []byte(streamID))
	writeLenPrefixed(h, []byte(cityID))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func writeLenPrefixed(h hash.Hash, b []byte) {
	var l [8]byte
	binary.BigEndian.PutUint64(l[:], uint64(len(b)))
	h.Write(l[:])
	h.Write(b)
}
