package graphstore

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore/canon"
)

const (
	testEngine = "lumen"
	testType   = "lumen.node.decision"
	otherType  = "lumen.effect.settled"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	s, err := Open(context.Background(), path, Options{CityID: "city-under-test"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.RegisterEventType(testEngine, testType)
	s.RegisterEventType(testEngine, otherType)
	return s
}

func canonPayload(t *testing.T, raw string) []byte {
	t.Helper()
	b, err := canon.Canonicalize([]byte(raw))
	if err != nil {
		t.Fatalf("canonicalize %q: %v", raw, err)
	}
	return b
}

// TestAppendExpectedVersionCAS_KillsS04 is the headline gate: two writers race to
// append at the same expectedVersion; exactly one wins, the other gets a loud
// ErrWrongExpectedVersion, the head advances by exactly one, and the survivor is
// intact. Run under -race.
func TestAppendExpectedVersionCAS_KillsS04(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const stream = "gcj-root-cas"

	payloadA := canonPayload(t, `{"who":"A"}`)
	payloadB := canonPayload(t, `{"who":"B"}`)

	type outcome struct {
		payload []byte
		res     AppendResult
		err     error
	}
	results := make([]outcome, 2)
	payloads := [2][]byte{payloadA, payloadB}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := s.Append(ctx, stream, testEngine, 0, 0, []JournalEvent{{
				Type:    testType,
				Payload: payloads[i],
			}})
			results[i] = outcome{payload: payloads[i], res: res, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	var winners, losers int
	var winnerPayload []byte
	for _, o := range results {
		switch {
		case o.err == nil:
			winners++
			winnerPayload = o.payload
			if o.res.FirstSeq != 1 {
				t.Fatalf("winner FirstSeq = %d, want 1", o.res.FirstSeq)
			}
		case errors.Is(o.err, ErrWrongExpectedVersion):
			losers++
		default:
			t.Fatalf("unexpected error: %v", o.err)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("winners=%d losers=%d, want exactly 1 each (silent loss or double-commit)", winners, losers)
	}

	head, err := s.Head(ctx, stream)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head != 1 {
		t.Fatalf("head = %d, want exactly 1 after the race", head)
	}

	events, err := s.ReadStream(ctx, stream, 1, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("stored %d events, want 1", len(events))
	}
	if string(events[0].Payload) != string(winnerPayload) {
		t.Fatalf("stored payload %q is not the winner's %q", events[0].Payload, winnerPayload)
	}
	if err := s.Verify(ctx, stream); err != nil {
		t.Fatalf("verify after race: %v", err)
	}
}

// TestAppendIdempotent proves R-IDEM: re-appending with the same idem token
// returns the existing seq via Duplicates and writes no new row.
func TestAppendIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const stream = "gcj-root-idem"

	ev := JournalEvent{Type: testType, IdemToken: "act:node-1:0", Payload: canonPayload(t, `{"n":1}`)}
	first, err := s.Append(ctx, stream, testEngine, 0, 0, []JournalEvent{ev})
	if err != nil {
		t.Fatalf("first append: %v", err)
	}
	if first.FirstSeq != 1 || len(first.Duplicates) != 0 {
		t.Fatalf("first append = %+v, want FirstSeq 1, no duplicates", first)
	}

	// Re-append the exact same token; expectedVersion is deliberately stale (0)
	// to model a crash-replay after the head already advanced.
	second, err := s.Append(ctx, stream, testEngine, 0, 0, []JournalEvent{ev})
	if err != nil {
		t.Fatalf("replay append: %v", err)
	}
	if got, ok := second.Duplicates[0]; !ok || got != 1 {
		t.Fatalf("replay duplicates = %+v, want {0:1}", second.Duplicates)
	}

	head, err := s.Head(ctx, stream)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head != 1 {
		t.Fatalf("head = %d after replay, want 1 (no new row)", head)
	}
}

// TestChainHashTamperEvidence proves the hash chain binds every payload: a clean
// chain verifies, and a divergent row re-inserted at an existing seq (via the
// retention gate) is detected at that exact seq.
func TestChainHashTamperEvidence(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const stream = "gcj-root-chain"

	for i := 0; i < 3; i++ {
		if _, err := s.Append(ctx, stream, testEngine, uint64(i), 0, []JournalEvent{{
			Type:    testType,
			Payload: canonPayload(t, `{"i":`+string(rune('0'+i))+`}`),
		}}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if err := s.Verify(ctx, stream); err != nil {
		t.Fatalf("verify clean chain: %v", err)
	}

	// Read seq 2's stored chain_hash so we can re-insert a divergent payload but
	// keep the STALE chain_hash — the recompute must then diverge.
	events, err := s.ReadStream(ctx, stream, 2, 2)
	if err != nil || len(events) != 1 {
		t.Fatalf("read seq 2: %v (n=%d)", err, len(events))
	}
	staleChain := events[0].ChainHash
	divergent := canonPayload(t, `{"tampered":true}`)
	divergentHash := canon.Hash(divergent)

	db := s.DB()
	if _, err := db.ExecContext(ctx, `INSERT INTO retention_gate(stream_id, max_seq) VALUES(?, 2)`, stream); err != nil {
		t.Fatalf("open gate: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM journal WHERE stream_id = ? AND seq = 2`, stream); err != nil {
		t.Fatalf("delete seq 2 under gate: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO journal
		   (stream_id, seq, substream, engine, type, ir_contract_version,
		    idem_token, payload, payload_hash, chain_hash, lease_epoch, appended_at)
		 VALUES (?, 2, '', ?, ?, '', NULL, ?, ?, ?, 0, ?)`,
		stream, testEngine, testType, divergent, divergentHash[:], staleChain[:],
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("re-insert divergent row: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM retention_gate WHERE stream_id = ?`, stream); err != nil {
		t.Fatalf("close gate: %v", err)
	}

	err = s.Verify(ctx, stream)
	if !errors.Is(err, ErrChainBroken) {
		t.Fatalf("verify tampered chain = %v, want ErrChainBroken", err)
	}
}

// TestAppendOnlyTriggerRejectsUpdateDelete pins SEC-3: UPDATE and ungated DELETE
// raise the append-only trigger; a DELETE gated by an open retention_gate at or
// below max_seq succeeds.
func TestAppendOnlyTriggerRejectsUpdateDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const stream = "gcj-root-trigger"

	if _, err := s.Append(ctx, stream, testEngine, 0, 0, []JournalEvent{{
		Type: testType, Payload: canonPayload(t, `{"x":1}`),
	}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	db := s.DB()

	_, err := db.ExecContext(ctx, `UPDATE journal SET payload = x'00' WHERE stream_id = ? AND seq = 1`, stream)
	if err == nil || !containsAppendOnly(err.Error()) {
		t.Fatalf("UPDATE journal = %v, want append-only rejection", err)
	}

	_, err = db.ExecContext(ctx, `DELETE FROM journal WHERE stream_id = ? AND seq = 1`, stream)
	if err == nil || !containsAppendOnly(err.Error()) {
		t.Fatalf("ungated DELETE journal = %v, want append-only rejection", err)
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO retention_gate(stream_id, max_seq) VALUES(?, 1)`, stream); err != nil {
		t.Fatalf("open gate: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM journal WHERE stream_id = ? AND seq = 1`, stream); err != nil {
		t.Fatalf("gated DELETE journal = %v, want success", err)
	}
	head, err := s.Head(ctx, stream)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head != 0 {
		t.Fatalf("head = %d after gated delete, want 0", head)
	}
}

func containsAppendOnly(msg string) bool {
	return len(msg) > 0 && (indexOf(msg, "append-only") >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestUnknownEventTypeRejected pins I-5: an unregistered (engine, type) is
// rejected at append.
func TestUnknownEventTypeRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.Append(ctx, "gcj-root-vocab", testEngine, 0, 0, []JournalEvent{{
		Type: "lumen.not.registered", Payload: canonPayload(t, `{}`),
	}})
	if !errors.Is(err, ErrUnknownEventType) {
		t.Fatalf("append unregistered type = %v, want ErrUnknownEventType", err)
	}
	head, err := s.Head(ctx, "gcj-root-vocab")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head != 0 {
		t.Fatalf("head = %d, want 0 (nothing written)", head)
	}
}

// TestLeaseFencing pins the fencing belt (02 §4.1): an append presenting a
// leaseEpoch below the current epoch is rejected; a valid epoch succeeds; and
// AcquireWriterLease advances the epoch monotonically.
func TestLeaseFencing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const stream = "gcj-root-lease"

	l1, err := s.AcquireWriterLease(ctx, stream, "ctrl-a", time.Minute)
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	if l1.Epoch != 1 {
		t.Fatalf("first epoch = %d, want 1", l1.Epoch)
	}

	if _, err := s.Append(ctx, stream, testEngine, 0, l1.Epoch, []JournalEvent{{
		Type: testType, Payload: canonPayload(t, `{"e":1}`),
	}}); err != nil {
		t.Fatalf("append under epoch 1: %v", err)
	}

	// The same controller re-acquires after a restart: allowed, and the epoch
	// advances monotonically, fencing its own prior instance.
	l2, err := s.AcquireWriterLease(ctx, stream, "ctrl-a", time.Minute)
	if err != nil {
		t.Fatalf("re-acquire (same holder): %v", err)
	}
	if l2.Epoch != 2 {
		t.Fatalf("second epoch = %d, want 2 (monotonic)", l2.Epoch)
	}

	// A fenced writer (stale epoch 1 < current 2) is rejected.
	_, err = s.Append(ctx, stream, testEngine, 1, l1.Epoch, []JournalEvent{{
		Type: testType, Payload: canonPayload(t, `{"e":"stale"}`),
	}})
	if !errors.Is(err, ErrLeaseFenced) {
		t.Fatalf("append under stale epoch = %v, want ErrLeaseFenced", err)
	}

	// The current holder (epoch 2) proceeds.
	if _, err := s.Append(ctx, stream, testEngine, 1, l2.Epoch, []JournalEvent{{
		Type: testType, Payload: canonPayload(t, `{"e":2}`),
	}}); err != nil {
		t.Fatalf("append under epoch 2: %v", err)
	}
}

// TestLeaseHeldByAnother verifies a live lease held by a different holder cannot
// be stolen until it expires.
func TestLeaseHeldByAnother(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const stream = "gcj-root-held"

	if _, err := s.AcquireWriterLease(ctx, stream, "holder-1", time.Hour); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	_, err := s.AcquireWriterLease(ctx, stream, "holder-2", time.Hour)
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("steal live lease = %v, want ErrLeaseHeld", err)
	}

	// Releasing lets a new holder acquire, and the epoch keeps climbing (never
	// resets) across the handoff.
	held := WriterLease{StreamID: stream, Holder: "holder-1", Epoch: 1}
	if err := s.ReleaseWriterLease(ctx, held); err != nil {
		t.Fatalf("release: %v", err)
	}
	l2, err := s.AcquireWriterLease(ctx, stream, "holder-2", time.Hour)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if l2.Epoch != 2 {
		t.Fatalf("epoch after handoff = %d, want 2 (monotonic, never reset)", l2.Epoch)
	}
}

// TestDenseSeqAndCanonRoundTrip pins dense 1..N sequencing and canonical
// encoding stability across the store boundary.
func TestDenseSeqAndCanonRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const stream = "gcj-root-dense"

	const n = 5
	for i := 0; i < n; i++ {
		res, err := s.Append(ctx, stream, testEngine, uint64(i), 0, []JournalEvent{{
			Type:    testType,
			Payload: canonPayload(t, `{"k":`+string(rune('0'+i))+`}`),
		}})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if res.FirstSeq != uint64(i+1) {
			t.Fatalf("append %d FirstSeq = %d, want %d", i, res.FirstSeq, i+1)
		}
	}

	events, err := s.ReadStream(ctx, stream, 1, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(events) != n {
		t.Fatalf("read %d events, want %d", len(events), n)
	}
	for i, e := range events {
		if e.Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d, want dense %d", i, e.Seq, i+1)
		}
		// The stored payload is byte-identical to a re-canonicalization of
		// itself (verbatim storage + canonical stability).
		again, err := canon.Canonicalize(e.Payload)
		if err != nil {
			t.Fatalf("re-canonicalize seq %d: %v", e.Seq, err)
		}
		if string(again) != string(e.Payload) {
			t.Fatalf("seq %d payload not canonical-stable: %q vs %q", e.Seq, again, e.Payload)
		}
	}

	// Key order at the input does not affect stored bytes.
	a := canonPayload(t, `{"b":2,"a":1}`)
	b := canonPayload(t, `{"a":1,"b":2}`)
	if string(a) != string(b) {
		t.Fatalf("canonical encoding is key-order sensitive: %q vs %q", a, b)
	}
}
