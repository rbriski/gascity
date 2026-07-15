package beads

import (
	"context"
	"errors"
)

var _ AtomicReadWriteHandleProvider = (*CachingStore)(nil)

// AtomicReadWriteHandle returns a cache-coherent atomic read/write handle when
// the backing store supports the capability. The CachingStore itself does not
// implement AtomicReadWriteStore, so an unsupported backing remains a typed
// absence rather than a runtime error or a fabricated transaction guarantee.
func (c *CachingStore) AtomicReadWriteHandle() (AtomicReadWriteStore, bool) {
	if c == nil {
		return nil, false
	}
	backing, ok := AtomicReadWriteFor(c.backing)
	if !ok {
		return nil, false
	}
	return cachingAtomicReadWriteStore{cache: c, backing: backing}, true
}

type cachingAtomicReadWriteStore struct {
	cache   *CachingStore
	backing AtomicReadWriteStore
}

func (s cachingAtomicReadWriteStore) AtomicReadWrite(ctx context.Context, commitMsg string, fn func(AtomicReadWriteTx) error) error {
	if ctx == nil {
		return errors.New("beads atomic read/write: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if fn == nil {
		return errors.New("beads atomic read/write: nil callback")
	}
	tx := newCachingAtomicReadWriteTx()
	if err := s.backing.AtomicReadWrite(ctx, commitMsg, func(backingTx AtomicReadWriteTx) error {
		tx.backing = backingTx
		return fn(tx)
	}); err != nil {
		return err
	}
	s.cache.refreshTxTouchedBeads(tx.ids, nil)
	return nil
}

type cachingAtomicReadWriteTx struct {
	backing AtomicReadWriteTx
	seen    map[string]struct{}
	ids     []string
}

func newCachingAtomicReadWriteTx() *cachingAtomicReadWriteTx {
	return &cachingAtomicReadWriteTx{seen: make(map[string]struct{})}
}

func (tx *cachingAtomicReadWriteTx) GetIssue(id string) (Bead, error) {
	return tx.backing.GetIssue(id)
}

func (tx *cachingAtomicReadWriteTx) Create(b Bead) (Bead, error) {
	created, err := tx.backing.Create(b)
	if err != nil {
		return Bead{}, err
	}
	tx.touch(created.ID)
	return created, nil
}

func (tx *cachingAtomicReadWriteTx) Update(id string, opts UpdateOpts) error {
	if err := tx.backing.Update(id, opts); err != nil {
		return err
	}
	tx.touch(id)
	return nil
}

func (tx *cachingAtomicReadWriteTx) GetMetadata(key string) (string, error) {
	return tx.backing.GetMetadata(key)
}

func (tx *cachingAtomicReadWriteTx) SetMetadata(key, value string) error {
	return tx.backing.SetMetadata(key, value)
}

func (tx *cachingAtomicReadWriteTx) touch(id string) {
	if id == "" {
		return
	}
	if _, ok := tx.seen[id]; ok {
		return
	}
	tx.seen[id] = struct{}{}
	tx.ids = append(tx.ids, id)
}
