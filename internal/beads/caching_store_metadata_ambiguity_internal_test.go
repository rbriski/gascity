package beads

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

type ambiguousMetadataBatchStore struct {
	Store
	err        error
	commitKeys []string
	getCalls   atomic.Int64
}

func (s *ambiguousMetadataBatchStore) SetMetadataBatch(id string, kvs map[string]string) error {
	committed := make(map[string]string, len(s.commitKeys))
	for _, key := range s.commitKeys {
		if value, ok := kvs[key]; ok {
			committed[key] = value
		}
	}
	if len(committed) > 0 {
		if err := s.Store.SetMetadataBatch(id, committed); err != nil {
			return err
		}
	}
	return s.err
}

func (s *ambiguousMetadataBatchStore) Get(id string) (Bead, error) {
	s.getCalls.Add(1)
	return s.Store.Get(id)
}

type capturedFullScanStore struct {
	Store
	armed    atomic.Bool
	captured chan struct{}
	resume   chan struct{}
	once     sync.Once
}

func (s *capturedFullScanStore) List(query ListQuery) ([]Bead, error) {
	rows, err := s.Store.List(query)
	if err != nil || !s.armed.Load() || !isCacheFullScanQuery(query) {
		return rows, err
	}
	s.once.Do(func() { close(s.captured) })
	<-s.resume
	return rows, nil
}

func isCacheFullScanQuery(query ListQuery) bool {
	return query.AllowScan && query.SkipLabels && !query.IncludeClosed &&
		query.TierMode == TierBoth && !query.Live && query.Status == "" &&
		query.Type == "" && query.Label == "" && query.Assignee == "" &&
		len(query.Assignees) == 0 && query.ParentID == "" &&
		len(query.ParentIDs) == 0 && len(query.Metadata) == 0 && query.Limit == 0
}

func waitForCacheTestSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func TestCachingStoreSetMetadataBatchErrorInvalidatesAndRereadsBackingTruth(t *testing.T) {
	patch := map[string]string{"state": "asleep", "state_reason": "healed"}
	for _, tc := range []struct {
		name       string
		commitKeys []string
	}{
		{name: "reject before write"},
		{name: "commit state only", commitKeys: []string{"state"}},
		{name: "commit reason only", commitKeys: []string{"state_reason"}},
		{name: "commit all", commitKeys: []string{"state", "state_reason"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := NewMemStore()
			created, err := mem.Create(Bead{
				Title:    "worker",
				Type:     "session",
				Status:   "open",
				Metadata: map[string]string{"state": "active", "state_reason": "old"},
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			wantErr := errors.New("ambiguous metadata batch")
			backing := &ambiguousMetadataBatchStore{Store: mem, err: wantErr, commitKeys: tc.commitKeys}
			var notifications atomic.Int64
			cache := NewCachingStoreForTest(backing, func(string, string, json.RawMessage) {
				notifications.Add(1)
			})
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}

			cache.mu.RLock()
			startSeq := cache.mutationSeq
			cachedBefore := cloneBead(cache.beads[created.ID])
			freshBefore := cache.lastFreshAt
			cache.mu.RUnlock()

			if err := cache.SetMetadataBatch(created.ID, patch); !errors.Is(err, wantErr) {
				t.Fatalf("SetMetadataBatch error = %v, want exact %v", err, wantErr)
			}

			cache.mu.RLock()
			_, dirty := cache.dirty[created.ID]
			_, local := cache.localBeadAt[created.ID]
			cachedAfter := cloneBead(cache.beads[created.ID])
			gotSeq := cache.mutationSeq
			rowSeq := cache.beadSeq[created.ID]
			freshAfter := cache.lastFreshAt
			cache.mu.RUnlock()
			if gotSeq != startSeq+1 || rowSeq != gotSeq || !dirty {
				t.Fatalf("ambiguity fence = mutation:%d row:%d dirty:%v, want mutation/row %d and dirty", gotSeq, rowSeq, dirty, startSeq+1)
			}
			if local {
				t.Fatal("ambiguous write stamped localBeadAt; foreign truth could be hidden")
			}
			if !reflect.DeepEqual(cachedAfter, cachedBefore) {
				t.Fatalf("ambiguous write changed cached row:\n got = %#v\nwant = %#v", cachedAfter, cachedBefore)
			}
			if !freshAfter.Equal(freshBefore) {
				t.Fatalf("ambiguous write marked cache fresh: before=%v after=%v", freshBefore, freshAfter)
			}
			if notifications.Load() != 0 {
				t.Fatalf("ambiguous write notifications = %d, want 0", notifications.Load())
			}
			if _, ok := cache.CachedList(ListQuery{Type: "session"}); ok {
				t.Fatal("CachedList served while ambiguous row was dirty")
			}

			wantTruth := cloneBead(created)
			for _, key := range tc.commitKeys {
				wantTruth.Metadata[key] = patch[key]
			}
			raw, err := mem.Get(created.ID)
			if err != nil {
				t.Fatalf("backing Get: %v", err)
			}
			if !reflect.DeepEqual(raw.Metadata, wantTruth.Metadata) {
				t.Fatalf("backing truth = %#v, want %#v", raw.Metadata, wantTruth.Metadata)
			}

			backing.getCalls.Store(0)
			rows, err := cache.List(ListQuery{Type: "session"})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if backing.getCalls.Load() != 1 {
				t.Fatalf("ordinary projection backing Gets = %d, want 1", backing.getCalls.Load())
			}
			if len(rows) != 1 || !reflect.DeepEqual(rows[0].Metadata, raw.Metadata) {
				t.Fatalf("ordinary projection = %#v, want backing truth %#v", rows, raw.Metadata)
			}
			cache.mu.RLock()
			_, stillDirty := cache.dirty[created.ID]
			_, stillFenced := cache.beadSeq[created.ID]
			cachedTruth := cloneBead(cache.beads[created.ID])
			cache.mu.RUnlock()
			if stillDirty || stillFenced || !reflect.DeepEqual(cachedTruth.Metadata, raw.Metadata) {
				t.Fatalf("ordinary projection did not install truth: dirty=%v fenced=%v cached=%#v raw=%#v", stillDirty, stillFenced, cachedTruth.Metadata, raw.Metadata)
			}
		})
	}
}

func TestCachingStoreSetMetadataBatchErrorFencesCapturedFullScan(t *testing.T) {
	patch := map[string]string{"state": "asleep", "state_reason": "healed"}
	for _, refresh := range []string{"prime", "reconcile"} {
		for _, tc := range []struct {
			name       string
			commitKeys []string
		}{
			{name: "partial", commitKeys: []string{"state"}},
			{name: "full", commitKeys: []string{"state", "state_reason"}},
		} {
			t.Run(refresh+"/"+tc.name, func(t *testing.T) {
				mem := NewMemStore()
				created, err := mem.Create(Bead{
					Title:    "worker",
					Type:     "session",
					Status:   "open",
					Metadata: map[string]string{"state": "active", "state_reason": "old"},
				})
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				wantErr := errors.New("ambiguous metadata batch")
				ambiguous := &ambiguousMetadataBatchStore{Store: mem, err: wantErr, commitKeys: tc.commitKeys}
				capturing := &capturedFullScanStore{
					Store: ambiguous, captured: make(chan struct{}), resume: make(chan struct{}),
				}
				cache := NewCachingStoreForTest(capturing, nil)
				if err := cache.Prime(context.Background()); err != nil {
					t.Fatalf("initial Prime: %v", err)
				}
				cache.mu.RLock()
				startSeq := cache.mutationSeq
				cachedBefore := cloneBead(cache.beads[created.ID])
				cache.mu.RUnlock()

				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(capturing.resume) }) }
				t.Cleanup(release)
				capturing.armed.Store(true)
				refreshDone := make(chan error, 1)
				go func() {
					if refresh == "prime" {
						refreshDone <- cache.Prime(context.Background())
						return
					}
					cache.runReconciliation()
					refreshDone <- nil
				}()
				waitForCacheTestSignal(t, capturing.captured, "old full-scan row capture")

				if err := cache.SetMetadataBatch(created.ID, patch); !errors.Is(err, wantErr) {
					t.Fatalf("SetMetadataBatch error = %v, want exact %v", err, wantErr)
				}
				cache.mu.RLock()
				fence := cache.beadSeq[created.ID]
				_, dirtyBeforeResume := cache.dirty[created.ID]
				_, localBeforeResume := cache.localBeadAt[created.ID]
				cache.mu.RUnlock()
				if fence <= startSeq || !dirtyBeforeResume || localBeforeResume {
					t.Fatalf("pre-resume fence = %d start=%d dirty=%v local=%v", fence, startSeq, dirtyBeforeResume, localBeforeResume)
				}

				release()
				select {
				case err := <-refreshDone:
					if err != nil {
						t.Fatalf("%s refresh: %v", refresh, err)
					}
				case <-time.After(testutil.GoroutineRaceTimeout):
					t.Fatalf("timed out waiting for %s refresh", refresh)
				}

				cache.mu.RLock()
				_, dirtyAfterRefresh := cache.dirty[created.ID]
				rowSeq := cache.beadSeq[created.ID]
				_, localAfterRefresh := cache.localBeadAt[created.ID]
				cachedAfterRefresh := cloneBead(cache.beads[created.ID])
				cache.mu.RUnlock()
				if !dirtyAfterRefresh || rowSeq != fence || localAfterRefresh {
					t.Fatalf("stale refresh erased fence: dirty=%v rowSeq=%d want=%d local=%v", dirtyAfterRefresh, rowSeq, fence, localAfterRefresh)
				}
				if !reflect.DeepEqual(cachedAfterRefresh.Metadata, cachedBefore.Metadata) {
					t.Fatalf("stale refresh installed captured row:\n got = %#v\nwant = %#v", cachedAfterRefresh.Metadata, cachedBefore.Metadata)
				}
				if _, ok := cache.CachedList(ListQuery{Type: "session"}); ok {
					t.Fatal("CachedList served after stale refresh while ambiguity remained")
				}

				raw, err := mem.Get(created.ID)
				if err != nil {
					t.Fatalf("backing Get: %v", err)
				}
				ambiguous.getCalls.Store(0)
				rows, err := cache.List(ListQuery{Type: "session"})
				if err != nil {
					t.Fatalf("ordinary List: %v", err)
				}
				if ambiguous.getCalls.Load() != 1 {
					t.Fatalf("ordinary projection backing Gets = %d, want 1", ambiguous.getCalls.Load())
				}
				if len(rows) != 1 || !reflect.DeepEqual(rows[0].Metadata, raw.Metadata) {
					t.Fatalf("ordinary projection = %#v, want backing truth %#v", rows, raw.Metadata)
				}
				cache.mu.RLock()
				_, finalDirty := cache.dirty[created.ID]
				_, finalFence := cache.beadSeq[created.ID]
				cache.mu.RUnlock()
				if finalDirty || finalFence {
					t.Fatalf("ordinary projection left ambiguity state: dirty=%v fenced=%v", finalDirty, finalFence)
				}
			})
		}
	}
}
