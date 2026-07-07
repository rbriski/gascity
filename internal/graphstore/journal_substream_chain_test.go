package graphstore

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore/canon"
)

// TestChainHashIncludesSubstream_S6 pins the DDL-freeze S6 decision: substream is
// folded into the chain-hash preimage. Two things are proved:
//
//  1. Directly, chainHash over two rows that differ ONLY in substream yields
//     different digests, while an identical substream reproduces the digest — so
//     substream is genuinely part of the preimage and the framing stays injective.
//  2. End to end, a retention-gated delete+reinsert that changes ONLY substream
//     (keeping payload, payload_hash, and the original chain_hash) is now caught
//     by Verify with ErrChainBroken. Before S6 this row differed only in the
//     excluded substream column and Verify would have passed — the regression this
//     test locks down.
func TestChainHashIncludesSubstream_S6(t *testing.T) {
	// (1) Direct preimage check.
	prev := [32]byte{1, 2, 3}
	ph := [32]byte{9, 9, 9}
	base := chainHash(prev, "gcj-root", 2, "lumen", "lumen.channel.emit", "", "ir-1", ph)
	withSub := chainHash(prev, "gcj-root", 2, "lumen", "lumen.channel.emit", "chan-a", "ir-1", ph)
	sameSub := chainHash(prev, "gcj-root", 2, "lumen", "lumen.channel.emit", "", "ir-1", ph)
	if base == withSub {
		t.Fatalf("chainHash ignores substream: '' and 'chan-a' produced the same digest")
	}
	if base != sameSub {
		t.Fatalf("chainHash not deterministic for equal inputs: %x vs %x", base, sameSub)
	}

	// (2) End-to-end Verify detection.
	s := newTestStore(t)
	ctx := context.Background()
	const stream = "gcj-root-substream"

	for i := 0; i < 3; i++ {
		if _, err := s.Append(ctx, stream, testEngine, uint64(i), 0, []JournalEvent{{
			Type:    testType,
			Payload: canonPayload(t, `{"i":`+strconv.Itoa(i)+`}`),
		}}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if err := s.Verify(ctx, stream); err != nil {
		t.Fatalf("verify clean chain: %v", err)
	}

	// Read seq 2's stored row: keep its payload and chain_hash, change ONLY the
	// substream on reinsertion.
	events, err := s.ReadStream(ctx, stream, 2, 2)
	if err != nil || len(events) != 1 {
		t.Fatalf("read seq 2: %v (n=%d)", err, len(events))
	}
	row := events[0]
	if row.Substream != "" {
		t.Fatalf("precondition: seq 2 substream = %q, want empty", row.Substream)
	}
	payloadHash := canon.Hash(row.Payload)

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
		 VALUES (?, 2, 'chan-a', ?, ?, ?, NULL, ?, ?, ?, 0, ?)`,
		stream, row.Engine, row.Type, row.IRContractVersion, row.Payload,
		payloadHash[:], row.ChainHash[:], time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("re-insert row with changed substream: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM retention_gate WHERE stream_id = ?`, stream); err != nil {
		t.Fatalf("close gate: %v", err)
	}

	err = s.Verify(ctx, stream)
	if !errors.Is(err, ErrChainBroken) {
		t.Fatalf("verify after substream-only change = %v, want ErrChainBroken (substream not chained?)", err)
	}
}
