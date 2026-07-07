package beads_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/graphstore/canon"
)

const (
	capTestEngine = "lumen"
	capTestType   = "lumen.node.decision"
)

// newJournalStoreForCapabilities opens a temp graphstore with a registered
// event type and returns both the raw *graphstore.Store (for assertions) and a
// JournalStore over it.
func newJournalStoreForCapabilities(t *testing.T) (*graphstore.Store, *beads.JournalStore) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	gs, err := graphstore.Open(context.Background(), path, graphstore.Options{CityID: "cap-city"})
	if err != nil {
		t.Fatalf("open graphstore: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() })
	gs.RegisterEventType(capTestEngine, capTestType)
	return gs, beads.NewJournalStore(gs)
}

func canonBytes(t *testing.T, raw string) []byte {
	t.Helper()
	b, err := canon.Canonicalize([]byte(raw))
	if err != nil {
		t.Fatalf("canonicalize %q: %v", raw, err)
	}
	return b
}

// TestJournalCapabilitiesReachableBareAndCachingWrapped is the lockstep test for
// Part B: it proves the append-log, expected-version CAS, and writer-lease
// capabilities are reachable via the *For probes on a bare JournalStore AND on a
// CachingStore-wrapped one, and that an append→read round-trips through the
// wrapper (the cache forwards, it does not mask or silently drop).
func TestJournalCapabilitiesReachableBareAndCachingWrapped(t *testing.T) {
	ctx := context.Background()
	_, js := newJournalStoreForCapabilities(t)
	cached := beads.NewCachingStoreForTest(js, nil)

	const stream = "gcg-root-cap"

	for _, tc := range []struct {
		name  string
		store beads.Store
	}{
		{"bare", js},
		{"cachingWrapped", cached},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			appendLog, ok := beads.AppendLogStoreFor(tc.store)
			if !ok {
				t.Fatalf("AppendLogStoreFor(%s) = false, want reachable", tc.name)
			}
			casReader, ok := beads.ConditionalVersionStoreFor(tc.store)
			if !ok {
				t.Fatalf("ConditionalVersionStoreFor(%s) = false, want reachable", tc.name)
			}
			leases, ok := beads.WriterLeaseStoreFor(tc.store)
			if !ok {
				t.Fatalf("WriterLeaseStoreFor(%s) = false, want reachable", tc.name)
			}

			// The two probed handles operate on one shared journal, so run each
			// wrapper's round-trip on its own stream to stay independent.
			streamID := stream + "-" + tc.name

			// Head starts at 0 (absent stream).
			head, err := casReader.StreamHead(ctx, streamID)
			if err != nil {
				t.Fatalf("StreamHead pre-append: %v", err)
			}
			if head != 0 {
				t.Fatalf("StreamHead pre-append = %d, want 0", head)
			}

			// Append conditioned on expectedVersion=0 (the CAS), then read back.
			payload := canonBytes(t, `{"claim":"`+tc.name+`"}`)
			res, err := appendLog.AppendEvent(ctx, streamID, capTestEngine, 0, 0, []graphstore.JournalEvent{{
				Type:    capTestType,
				Payload: payload,
			}})
			if err != nil {
				t.Fatalf("AppendEvent: %v", err)
			}
			if res.FirstSeq != 1 {
				t.Fatalf("AppendEvent FirstSeq = %d, want 1", res.FirstSeq)
			}

			events, err := appendLog.ReadStream(ctx, streamID, 1, 0)
			if err != nil {
				t.Fatalf("ReadStream: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("ReadStream returned %d events, want 1", len(events))
			}
			if events[0].Type != capTestType || string(events[0].Payload) != string(payload) {
				t.Fatalf("round-trip mismatch: type=%q payload=%q", events[0].Type, events[0].Payload)
			}

			// Head advanced to 1 through the same probed reader.
			head, err = casReader.StreamHead(ctx, streamID)
			if err != nil {
				t.Fatalf("StreamHead post-append: %v", err)
			}
			if head != 1 {
				t.Fatalf("StreamHead post-append = %d, want 1", head)
			}

			// The CAS fails loudly on a stale expectedVersion — never a silent
			// overwrite.
			if _, err := appendLog.AppendEvent(ctx, streamID, capTestEngine, 0, 0, []graphstore.JournalEvent{{
				Type:    capTestType,
				Payload: canonBytes(t, `{"stale":true}`),
			}}); !errors.Is(err, graphstore.ErrWrongExpectedVersion) {
				t.Fatalf("stale AppendEvent err = %v, want ErrWrongExpectedVersion", err)
			}

			// Writer lease acquire/renew/release round-trips through the wrapper.
			lease, err := leases.AcquireWriterLease(ctx, streamID, "holder-"+tc.name, time.Minute)
			if err != nil {
				t.Fatalf("AcquireWriterLease: %v", err)
			}
			if lease.Epoch != 1 {
				t.Fatalf("lease epoch = %d, want 1", lease.Epoch)
			}
			renewed, err := leases.RenewWriterLease(ctx, lease, time.Minute)
			if err != nil {
				t.Fatalf("RenewWriterLease: %v", err)
			}
			if renewed.Epoch != lease.Epoch {
				t.Fatalf("renew changed epoch %d -> %d", lease.Epoch, renewed.Epoch)
			}
			if err := leases.ReleaseWriterLease(ctx, renewed); err != nil {
				t.Fatalf("ReleaseWriterLease: %v", err)
			}
		})
	}
}

// TestJournalCapabilityProbesAbsentOnNonJournalBacking proves the *For probes
// return an honest (nil, false) — never a silently degraded stub — when neither
// the store nor its wrapped backing supports the journal capabilities.
func TestJournalCapabilityProbesAbsentOnNonJournalBacking(t *testing.T) {
	mem := beads.NewMemStore()
	cached := beads.NewCachingStoreForTest(mem, nil)

	for _, tc := range []struct {
		name  string
		store beads.Store
	}{
		{"bareMem", mem},
		{"cachingWrappedMem", cached},
	} {
		if got, ok := beads.AppendLogStoreFor(tc.store); ok || got != nil {
			t.Fatalf("AppendLogStoreFor(%s) = (%v, %v), want (nil, false)", tc.name, got, ok)
		}
		if got, ok := beads.ConditionalVersionStoreFor(tc.store); ok || got != nil {
			t.Fatalf("ConditionalVersionStoreFor(%s) = (%v, %v), want (nil, false)", tc.name, got, ok)
		}
		if got, ok := beads.WriterLeaseStoreFor(tc.store); ok || got != nil {
			t.Fatalf("WriterLeaseStoreFor(%s) = (%v, %v), want (nil, false)", tc.name, got, ok)
		}
	}
}
