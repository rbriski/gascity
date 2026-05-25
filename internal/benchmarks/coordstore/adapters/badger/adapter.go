// Package badger provides a BadgerDB-backed StoreAdapter for the
// coordination-store benchmark sweep.
package badger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore"
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/internal/memstore"
)

var (
	keyMainPrefix      = []byte("main:")
	keyEphemeralPrefix = []byte("eph:")
	keyDepPrefix       = []byte("dep:")
)

const gcInterval = 30 * time.Second

// Adapter stores records in BadgerDB and serves reads through an in-process hot
// index loaded from the database at Open time.
type Adapter struct {
	*memstore.Adapter

	mu     sync.RWMutex
	db     *badger.DB
	stopGC chan struct{}
	gcDone chan struct{}

	gcRuns      atomic.Int64
	gcNoRewrite atomic.Int64
	gcErrors    atomic.Int64

	gcEvents              atomic.Int64
	gcLastStartedUnixNano atomic.Int64
	gcLastDurationNanos   atomic.Int64
	gcLastFreedBytes      atomic.Int64
	gcTotalFreedBytes     atomic.Int64
}

// New returns an uninitialized BadgerDB adapter.
func New() *Adapter {
	a := &Adapter{}
	a.Adapter = memstore.New("bdg", a)
	return a
}

// Open initializes BadgerDB and loads its current contents.
func (a *Adapter) Open(ctx context.Context, cfg coordstore.Config) error {
	path := filepath.Join(cfg.DataDir, "badger")
	opts := badger.DefaultOptions(path).WithLogger(nil)
	db, err := badger.Open(opts)
	if err != nil {
		return fmt.Errorf("badger: open %s: %w", path, err)
	}

	a.mu.Lock()
	a.db = db
	a.mu.Unlock()

	records, deps, err := a.load(ctx)
	if err != nil {
		db.Close() //nolint:errcheck
		a.mu.Lock()
		a.db = nil
		a.mu.Unlock()
		return err
	}
	a.ReplaceState(records, deps)
	a.startGC()
	return nil
}

// Close releases the BadgerDB handle.
func (a *Adapter) Close() error {
	a.stopBackgroundGC()

	a.mu.Lock()
	db := a.db
	a.db = nil
	a.mu.Unlock()
	if db == nil {
		return nil
	}
	return db.Close()
}

// SaveRecord writes a complete record atomically.
func (a *Adapter) SaveRecord(_ context.Context, r coordstore.Record) error {
	db := a.openDB()
	if db == nil {
		return fmt.Errorf("badger: database is not open")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("badger: marshal record %q: %w", r.ID, err)
	}

	return db.Update(func(txn *badger.Txn) error {
		dst := mainKey(r.ID)
		other := ephemeralKey(r.ID)
		if r.Ephemeral {
			dst = ephemeralKey(r.ID)
			other = mainKey(r.ID)
		}
		if err := deleteIfPresent(txn, other); err != nil {
			return err
		}
		entry := badger.NewEntry(dst, data)
		if r.Ephemeral && !r.ExpiresAt.IsZero() {
			if ttl := time.Until(r.ExpiresAt); ttl > 0 {
				entry = entry.WithTTL(ttl)
			}
		}
		if err := txn.SetEntry(entry); err != nil {
			return fmt.Errorf("badger: save record %q: %w", r.ID, err)
		}
		return nil
	})
}

// DeleteRecord removes a record from BadgerDB.
func (a *Adapter) DeleteRecord(_ context.Context, id string, _ bool) error {
	db := a.openDB()
	if db == nil {
		return fmt.Errorf("badger: database is not open")
	}
	return db.Update(func(txn *badger.Txn) error {
		if err := deleteIfPresent(txn, mainKey(id)); err != nil {
			return err
		}
		return deleteIfPresent(txn, ephemeralKey(id))
	})
}

// SaveDep writes a dependency edge.
func (a *Adapter) SaveDep(_ context.Context, d coordstore.Dep) error {
	db := a.openDB()
	if db == nil {
		return fmt.Errorf("badger: database is not open")
	}
	data, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("badger: marshal dep %q -> %q: %w", d.FromID, d.ToID, err)
	}
	return db.Update(func(txn *badger.Txn) error {
		if err := txn.Set(depKey(d.FromID, d.ToID), data); err != nil {
			return fmt.Errorf("badger: save dep %q -> %q: %w", d.FromID, d.ToID, err)
		}
		return nil
	})
}

// DeleteDep removes a dependency edge.
func (a *Adapter) DeleteDep(_ context.Context, fromID, toID string) error {
	db := a.openDB()
	if db == nil {
		return fmt.Errorf("badger: database is not open")
	}
	return db.Update(func(txn *badger.Txn) error {
		return deleteIfPresent(txn, depKey(fromID, toID))
	})
}

// ResetBacking wipes all BadgerDB keys.
func (a *Adapter) ResetBacking(context.Context) error {
	db := a.openDB()
	if db == nil {
		return fmt.Errorf("badger: database is not open")
	}
	if err := db.DropAll(); err != nil {
		return fmt.Errorf("badger: drop all: %w", err)
	}
	return nil
}

// PurgeExpired removes expired records from the hot index and asks BadgerDB to
// reclaim obsolete value-log space opportunistically.
func (a *Adapter) PurgeExpired(ctx context.Context) (int, error) {
	n, err := a.Adapter.PurgeExpired(ctx)
	if gcErr := a.runValueLogGC(0.5); gcErr != nil && err == nil {
		err = gcErr
	}
	return n, err
}

// Stats returns memstore counts plus BadgerDB storage and GC counters.
func (a *Adapter) Stats(ctx context.Context) map[string]int64 {
	stats := a.Adapter.Stats(ctx)
	db := a.openDB()
	if db != nil {
		lsm, vlog := db.Size()
		stats["badger_lsm_size_bytes"] = lsm
		stats["badger_vlog_size_bytes"] = vlog
	}
	stats["badger_gc_runs"] = a.gcRuns.Load()
	stats["badger_gc_no_rewrite"] = a.gcNoRewrite.Load()
	stats["badger_gc_errors"] = a.gcErrors.Load()
	stats["badger_gc_events"] = a.gcEvents.Load()
	stats["badger_gc_last_started_unix_nano"] = a.gcLastStartedUnixNano.Load()
	stats["badger_gc_last_duration_nanos"] = a.gcLastDurationNanos.Load()
	stats["badger_gc_last_freed_bytes"] = a.gcLastFreedBytes.Load()
	stats["badger_gc_total_freed_bytes"] = a.gcTotalFreedBytes.Load()
	return stats
}

func (a *Adapter) load(context.Context) ([]coordstore.Record, []coordstore.Dep, error) {
	db := a.openDB()
	if db == nil {
		return nil, nil, fmt.Errorf("badger: database is not open")
	}

	var records []coordstore.Record
	var deps []coordstore.Dep
	now := time.Now()
	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = true
		it := txn.NewIterator(opts)
		defer it.Close()

		for _, prefix := range [][]byte{keyMainPrefix, keyEphemeralPrefix} {
			for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
				item := it.Item()
				if err := item.Value(func(value []byte) error {
					var r coordstore.Record
					if err := json.Unmarshal(value, &r); err != nil {
						return fmt.Errorf("badger: unmarshal record %q: %w", item.KeyCopy(nil), err)
					}
					if r.Ephemeral && !r.ExpiresAt.IsZero() && !r.ExpiresAt.After(now) {
						return nil
					}
					records = append(records, r)
					return nil
				}); err != nil {
					return err
				}
			}
		}
		for it.Seek(keyDepPrefix); it.ValidForPrefix(keyDepPrefix); it.Next() {
			item := it.Item()
			if err := item.Value(func(value []byte) error {
				var d coordstore.Dep
				if err := json.Unmarshal(value, &d); err != nil {
					return fmt.Errorf("badger: unmarshal dep %q: %w", item.KeyCopy(nil), err)
				}
				deps = append(deps, d)
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("badger: load: %w", err)
	}
	return records, deps, nil
}

func (a *Adapter) openDB() *badger.DB {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.db
}

func (a *Adapter) startGC() {
	a.mu.Lock()
	if a.stopGC != nil {
		a.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	a.stopGC = stop
	a.gcDone = done
	a.mu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(gcInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = a.runValueLogGC(0.7)
			case <-stop:
				return
			}
		}
	}()
}

func (a *Adapter) stopBackgroundGC() {
	a.mu.Lock()
	stop := a.stopGC
	done := a.gcDone
	a.stopGC = nil
	a.gcDone = nil
	a.mu.Unlock()

	if stop == nil {
		return
	}
	close(stop)
	<-done
}

func (a *Adapter) runValueLogGC(discardRatio float64) error {
	db := a.openDB()
	if db == nil {
		return nil
	}
	started := time.Now()
	beforeLSM, beforeVlog := db.Size()
	err := db.RunValueLogGC(discardRatio)
	afterLSM, afterVlog := db.Size()
	a.recordGCEvent(started, beforeLSM+beforeVlog, afterLSM+afterVlog)
	if err == nil {
		a.gcRuns.Add(1)
		return nil
	}
	if errors.Is(err, badger.ErrNoRewrite) {
		a.gcNoRewrite.Add(1)
		return nil
	}
	a.gcErrors.Add(1)
	return fmt.Errorf("badger: value log gc: %w", err)
}

func (a *Adapter) recordGCEvent(started time.Time, beforeBytes, afterBytes int64) {
	freed := beforeBytes - afterBytes
	if freed < 0 {
		freed = 0
	}
	a.gcEvents.Add(1)
	a.gcLastStartedUnixNano.Store(started.UnixNano())
	a.gcLastDurationNanos.Store(time.Since(started).Nanoseconds())
	a.gcLastFreedBytes.Store(freed)
	a.gcTotalFreedBytes.Add(freed)
}

func deleteIfPresent(txn *badger.Txn, key []byte) error {
	err := txn.Delete(key)
	if err == nil || errors.Is(err, badger.ErrKeyNotFound) {
		return nil
	}
	return err
}

func mainKey(id string) []byte {
	key := make([]byte, 0, len(keyMainPrefix)+len(id))
	key = append(key, keyMainPrefix...)
	key = append(key, id...)
	return key
}

func ephemeralKey(id string) []byte {
	key := make([]byte, 0, len(keyEphemeralPrefix)+len(id))
	key = append(key, keyEphemeralPrefix...)
	key = append(key, id...)
	return key
}

func depKey(fromID, toID string) []byte {
	key := make([]byte, 0, len(keyDepPrefix)+len(fromID)+len(toID)+1)
	key = append(key, keyDepPrefix...)
	key = append(key, fromID...)
	key = append(key, ':')
	key = append(key, toID...)
	return key
}
