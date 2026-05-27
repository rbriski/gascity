package beads

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	bbolt "go.etcd.io/bbolt"
)

var (
	bboltBucketRecords  = []byte("records")
	bboltBucketWisps    = []byte("wisps")
	bboltBucketDeps     = []byte("deps")
	bboltBucketMetadata = []byte("metadata")

	bboltMetaSeq   = []byte("seq")
	bboltMetaOrder = []byte("order")
)

// BboltStore is a bbolt-backed Store implementation with an in-memory hot
// index. It loads all bbolt state at open time, serves reads from memory, and
// commits every mutation to bbolt before updating the hot index.
type BboltStore struct {
	mu sync.RWMutex

	path   string
	prefix string
	seq    int
	db     *bbolt.DB
	closed bool

	main      map[string]Bead
	wisps     map[string]Bead
	order     []string
	orderSeen map[string]bool
	deps      []Dep
	mainIdx   hqTierIndex
	wispIdx   hqTierIndex
}

type bboltStoreOptions struct {
	prefix string
}

// BboltStoreOption customizes OpenBboltStore.
type BboltStoreOption func(*bboltStoreOptions)

// WithBboltStoreIDPrefix sets the generated ID prefix. Empty keeps the
// default "gc" prefix.
func WithBboltStoreIDPrefix(prefix string) BboltStoreOption {
	return func(o *bboltStoreOptions) {
		if prefix != "" {
			o.prefix = prefix
		}
	}
}

// OpenBboltStore opens or creates a bbolt-backed bead store at path.
func OpenBboltStore(path string, opts ...BboltStoreOption) (*BboltStore, error) {
	cfg := bboltStoreOptions{prefix: "gc"}
	for _, opt := range opts {
		opt(&cfg)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("opening bbolt store: %w", err)
	}
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	store := &BboltStore{
		path:   path,
		prefix: cfg.prefix,
		db:     db,
	}
	store.resetCoreLocked()
	if err := store.initBuckets(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.load(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Shutdown closes the bbolt handle. It is idempotent.
func (s *BboltStore) Shutdown() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Create persists a new bead and its generated ID sequence.
func (s *BboltStore) Create(b Bead) (Bead, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return Bead{}, err
	}

	stored, nextSeq := s.normalizeCreateCandidateLocked(b)
	if _, ok := s.findLocked(stored.ID); ok {
		return Bead{}, fmt.Errorf("creating bead %q: duplicate id", stored.ID)
	}
	nextDeps := snapshotHQDeps(s.deps)
	for _, dep := range depsFromNeeds(stored) {
		nextDeps = bboltDepAdd(nextDeps, dep.IssueID, dep.DependsOnID, dep.Type)
	}
	nextOrder := s.orderWithIDLocked(stored.ID)

	if err := s.db.Update(func(tx *bbolt.Tx) error {
		if err := bboltPutBead(tx, stored); err != nil {
			return err
		}
		if err := bboltPutSeq(tx, nextSeq); err != nil {
			return err
		}
		if err := bboltPutOrder(tx, nextOrder); err != nil {
			return err
		}
		return bboltPersistChangedDepPairs(tx, s.deps, nextDeps)
	}); err != nil {
		return Bead{}, fmt.Errorf("creating bead %q: %w", stored.ID, err)
	}

	s.seq = nextSeq
	s.upsertOwnedLocked(stored)
	s.deps = nextDeps
	return cloneBead(stored), nil
}

// Get retrieves a bead by ID.
func (s *BboltStore) Get(id string) (Bead, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.ensureOpenLocked(); err != nil {
		return Bead{}, err
	}
	if b, ok := s.main[id]; ok {
		return cloneBead(b), nil
	}
	if b, ok := s.wisps[id]; ok {
		return cloneBead(b), nil
	}
	return Bead{}, fmt.Errorf("getting bead %q: %w", id, ErrNotFound)
}

// Update modifies fields of an existing bead.
func (s *BboltStore) Update(id string, opts UpdateOpts) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return err
	}
	b, ok := s.findLocked(id)
	if !ok {
		return fmt.Errorf("updating bead %q: %w", id, ErrNotFound)
	}
	applyHQUpdate(&b, opts)
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		return bboltPutBead(tx, b)
	}); err != nil {
		return fmt.Errorf("updating bead %q: %w", id, err)
	}
	s.upsertOwnedLocked(b)
	return nil
}

// Close sets a bead's status to closed.
func (s *BboltStore) Close(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return err
	}
	b, ok := s.findLocked(id)
	if !ok {
		return fmt.Errorf("closing bead %q: %w", id, ErrNotFound)
	}
	if b.Status == "closed" {
		return nil
	}
	b.Status = "closed"
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		return bboltPutBead(tx, b)
	}); err != nil {
		return fmt.Errorf("closing bead %q: %w", id, err)
	}
	s.upsertOwnedLocked(b)
	return nil
}

// Reopen sets a closed bead's status back to open.
func (s *BboltStore) Reopen(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return err
	}
	b, ok := s.findLocked(id)
	if !ok {
		return fmt.Errorf("reopening bead %q: %w", id, ErrNotFound)
	}
	if b.Status == "open" {
		return nil
	}
	b.Status = "open"
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		return bboltPutBead(tx, b)
	}); err != nil {
		return fmt.Errorf("reopening bead %q: %w", id, err)
	}
	s.upsertOwnedLocked(b)
	return nil
}

// CloseAll closes multiple beads and applies metadata to each changed bead.
func (s *BboltStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return 0, err
	}

	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	changed := make([]Bead, 0, len(idSet))
	for id := range idSet {
		b, ok := s.findLocked(id)
		if !ok || b.Status == "closed" {
			continue
		}
		b.Status = "closed"
		if len(metadata) > 0 {
			if b.Metadata == nil {
				b.Metadata = make(map[string]string, len(metadata))
			}
			for k, v := range metadata {
				b.Metadata[k] = v
			}
		}
		changed = append(changed, b)
	}
	if len(changed) == 0 {
		return 0, nil
	}
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		for _, b := range changed {
			if err := bboltPutBead(tx, b); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return 0, fmt.Errorf("closing beads: %w", err)
	}
	for _, b := range changed {
		s.upsertOwnedLocked(b)
	}
	return len(changed), nil
}

// List returns beads matching the query.
func (s *BboltStore) List(query ListQuery) ([]Bead, error) {
	if !query.HasFilter() && !query.AllowScan {
		return nil, fmt.Errorf("listing beads: %w", ErrQueryRequiresScan)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.ensureOpenLocked(); err != nil {
		return nil, err
	}
	candidates := s.candidateIDsLocked(query)
	snapshot := make([]Bead, 0, len(candidates))
	for _, id := range s.iterationIDsLocked(query, candidates) {
		if _, ok := candidates[id]; !ok {
			continue
		}
		if b, ok := s.main[id]; ok {
			snapshot = append(snapshot, cloneBead(b))
			continue
		}
		if b, ok := s.wisps[id]; ok {
			snapshot = append(snapshot, cloneBead(b))
		}
	}
	result := make([]Bead, 0, len(snapshot))
	for _, b := range snapshot {
		if query.Matches(b) {
			result = append(result, b)
		}
	}
	sortBeadsForQuery(result, query.Sort)
	if query.Limit > 0 && len(result) > query.Limit {
		result = result[:query.Limit]
	}
	return result, nil
}

// ListOpen returns non-closed beads in creation order by default.
func (s *BboltStore) ListOpen(status ...string) ([]Bead, error) {
	query := ListQuery{AllowScan: true}
	if len(status) > 0 {
		query.Status = status[0]
	}
	return s.List(query)
}

// Ready returns all open, unblocked actionable main-tier beads.
func (s *BboltStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	q := readyQueryFromArgs(query)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.ensureOpenLocked(); err != nil {
		return nil, err
	}

	candidateIDs := s.readyCandidateIDsLocked(q)
	candidateSet := make(map[string]bool, len(candidateIDs))
	snapshot := make([]Bead, 0, len(candidateIDs))
	for _, id := range candidateIDs {
		b, ok := s.main[id]
		if !ok {
			continue
		}
		candidateSet[id] = true
		snapshot = append(snapshot, cloneBead(b))
	}
	statusByID := make(map[string]string, len(s.main)+len(s.wisps))
	for id, b := range s.main {
		statusByID[id] = b.Status
	}
	for id, b := range s.wisps {
		statusByID[id] = b.Status
	}
	deps := make([]Dep, 0, len(s.deps))
	for _, dep := range s.deps {
		if candidateSet[dep.IssueID] {
			deps = append(deps, dep)
		}
	}

	var result []Bead
	for _, b := range snapshot {
		if b.Status != "open" {
			continue
		}
		if q.Assignee != "" && b.Assignee != q.Assignee {
			continue
		}
		if IsReadyExcludedType(b.Type) || hqBlockedBySnapshot(b.ID, deps, statusByID) {
			continue
		}
		result = append(result, b)
		if q.Limit > 0 && len(result) >= q.Limit {
			break
		}
	}
	return result, nil
}

// Children returns children of parentID.
func (s *BboltStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		ParentID:      parentID,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedAsc,
	})
}

// ListByLabel returns beads matching a label.
func (s *BboltStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		Label:         label,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedDesc,
		TierMode:      TierModeFromOpts(opts),
	})
}

// ListByAssignee returns beads assigned to assignee with status.
func (s *BboltStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	return s.List(ListQuery{
		Assignee: assignee,
		Status:   status,
		Limit:    limit,
		Sort:     SortCreatedDesc,
	})
}

// ListByMetadata returns beads whose metadata contains all filters.
func (s *BboltStore) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		Metadata:      filters,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedDesc,
		TierMode:      TierModeFromOpts(opts),
	})
}

// SetMetadata sets a single metadata key-value pair.
func (s *BboltStore) SetMetadata(id, key, value string) error {
	return s.SetMetadataBatch(id, map[string]string{key: value})
}

// SetMetadataBatch atomically merges metadata into a bead.
func (s *BboltStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if len(kvs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return err
	}
	b, ok := s.findLocked(id)
	if !ok {
		return fmt.Errorf("setting metadata batch on %q: %w", id, ErrNotFound)
	}
	if b.Metadata == nil {
		b.Metadata = make(map[string]string, len(kvs))
	}
	for k, v := range kvs {
		b.Metadata[k] = v
	}
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		return bboltPutBead(tx, b)
	}); err != nil {
		return fmt.Errorf("setting metadata batch on %q: %w", id, err)
	}
	s.upsertOwnedLocked(b)
	return nil
}

// Tx executes fn against the BboltStore write surface.
func (s *BboltStore) Tx(_ string, fn func(tx Tx) error) error {
	return runSequentialTx(s, fn)
}

// Delete permanently removes a bead and dependency edges touching it.
func (s *BboltStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return err
	}
	if _, ok := s.findLocked(id); !ok {
		return fmt.Errorf("deleting bead %q: %w", id, ErrNotFound)
	}
	nextDeps := bboltDepsWithoutID(s.deps, id)
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		if err := bboltDeleteBead(tx, id); err != nil {
			return err
		}
		return bboltPersistChangedDepPairs(tx, s.deps, nextDeps)
	}); err != nil {
		return fmt.Errorf("deleting bead %q: %w", id, err)
	}
	s.deleteLocked(id)
	s.deps = nextDeps
	return nil
}

// Ping verifies that the bbolt handle is accessible.
func (s *BboltStore) Ping() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.db == nil {
		return fmt.Errorf("pinging bbolt store: closed")
	}
	if err := s.db.View(func(*bbolt.Tx) error { return nil }); err != nil {
		return fmt.Errorf("pinging bbolt store: %w", err)
	}
	return nil
}

// DepAdd records a dependency.
func (s *BboltStore) DepAdd(issueID, dependsOnID, depType string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return err
	}
	nextDeps := bboltDepAdd(s.deps, issueID, dependsOnID, depType)
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		return bboltPersistChangedDepPairs(tx, s.deps, nextDeps)
	}); err != nil {
		return fmt.Errorf("adding dependency %q -> %q: %w", issueID, dependsOnID, err)
	}
	s.deps = nextDeps
	return nil
}

// DepRemove removes a dependency between two beads.
func (s *BboltStore) DepRemove(issueID, dependsOnID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return err
	}
	nextDeps := bboltDepRemove(s.deps, issueID, dependsOnID)
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		return bboltPersistChangedDepPairs(tx, s.deps, nextDeps)
	}); err != nil {
		return fmt.Errorf("removing dependency %q -> %q: %w", issueID, dependsOnID, err)
	}
	s.deps = nextDeps
	return nil
}

// DepList returns dependencies in the requested direction.
func (s *BboltStore) DepList(id, direction string) ([]Dep, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.ensureOpenLocked(); err != nil {
		return nil, err
	}
	var result []Dep
	for _, d := range s.deps {
		switch direction {
		case "up":
			if d.DependsOnID == id {
				result = append(result, d)
			}
		default:
			if d.IssueID == id {
				result = append(result, d)
			}
		}
	}
	return result, nil
}

// PurgeExpired removes expired wisps and expired closed main-tier beads.
func (s *BboltStore) PurgeExpired() (int, error) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return 0, err
	}
	var ids []string
	for id, bead := range s.wisps {
		expiresAt, ok := hqBeadExpiresAt(bead)
		if ok && !expiresAt.After(now) {
			ids = append(ids, id)
		}
	}
	for id, bead := range s.main {
		if hqClosedTaskExpired(bead, now, hqDefaultClosedTaskRetention) && !s.hasOpenChildrenLocked(id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	nextDeps := snapshotHQDeps(s.deps)
	for _, id := range ids {
		nextDeps = bboltDepsWithoutID(nextDeps, id)
	}
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		for _, id := range ids {
			if err := bboltDeleteBead(tx, id); err != nil {
				return err
			}
		}
		return bboltPersistChangedDepPairs(tx, s.deps, nextDeps)
	}); err != nil {
		return 0, fmt.Errorf("purging expired beads: %w", err)
	}
	for _, id := range ids {
		s.deleteLocked(id)
	}
	s.deps = nextDeps
	return len(ids), nil
}

func (s *BboltStore) initBuckets() error {
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bboltBucketRecords, bboltBucketWisps, bboltBucketDeps, bboltBucketMetadata} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		meta := tx.Bucket(bboltBucketMetadata)
		if meta.Get(bboltMetaSeq) == nil {
			if err := meta.Put(bboltMetaSeq, bboltEncodeSeq(0)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("opening bbolt store: create buckets: %w", err)
	}
	return nil
}

func (s *BboltStore) load() error {
	var records []Bead
	var deps []Dep
	var seq int
	var order []string
	if err := s.db.View(func(tx *bbolt.Tx) error {
		for _, spec := range []struct {
			name      []byte
			ephemeral bool
		}{
			{name: bboltBucketRecords},
			{name: bboltBucketWisps, ephemeral: true},
		} {
			if err := tx.Bucket(spec.name).ForEach(func(_, value []byte) error {
				var b Bead
				if err := json.Unmarshal(value, &b); err != nil {
					return err
				}
				b.Ephemeral = spec.ephemeral
				records = append(records, b)
				return nil
			}); err != nil {
				return err
			}
		}
		if err := tx.Bucket(bboltBucketDeps).ForEach(func(_, value []byte) error {
			loaded, err := bboltDecodeDeps(value)
			if err != nil {
				return err
			}
			deps = append(deps, loaded...)
			return nil
		}); err != nil {
			return err
		}
		meta := tx.Bucket(bboltBucketMetadata)
		seq = bboltDecodeSeq(meta.Get(bboltMetaSeq))
		if data := meta.Get(bboltMetaOrder); len(data) > 0 {
			if err := json.Unmarshal(data, &order); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("opening bbolt store: load: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.resetCoreLocked()
	s.seq = seq
	s.loadRecordsLocked(records, order)
	s.deps = snapshotHQDeps(deps)
	if s.seq < s.maxNumericSuffixLocked() {
		s.seq = s.maxNumericSuffixLocked()
	}
	return nil
}

func (s *BboltStore) resetCoreLocked() {
	s.main = make(map[string]Bead)
	s.wisps = make(map[string]Bead)
	s.order = nil
	s.orderSeen = make(map[string]bool)
	s.deps = nil
	s.mainIdx = newHQTierIndex()
	s.wispIdx = newHQTierIndex()
	s.seq = 0
}

func (s *BboltStore) loadRecordsLocked(records []Bead, order []string) {
	byID := make(map[string]Bead, len(records))
	for _, b := range records {
		byID[b.ID] = b
	}
	for _, id := range order {
		b, ok := byID[id]
		if !ok {
			continue
		}
		s.upsertOwnedLocked(cloneBead(b))
		delete(byID, id)
	}
	if len(byID) == 0 {
		return
	}
	remaining := make([]Bead, 0, len(byID))
	for _, b := range byID {
		remaining = append(remaining, b)
	}
	sort.Slice(remaining, func(i, j int) bool {
		if remaining[i].CreatedAt.Equal(remaining[j].CreatedAt) {
			return remaining[i].ID < remaining[j].ID
		}
		return remaining[i].CreatedAt.Before(remaining[j].CreatedAt)
	})
	for _, b := range remaining {
		s.upsertOwnedLocked(cloneBead(b))
	}
}

func (s *BboltStore) ensureOpenLocked() error {
	if s.closed || s.db == nil {
		return fmt.Errorf("bbolt store is closed")
	}
	return nil
}

func (s *BboltStore) normalizeCreateCandidateLocked(b Bead) (Bead, int) {
	b = cloneBead(b)
	nextSeq := s.seq
	if b.ID == "" {
		nextSeq++
		b.ID = fmt.Sprintf("%s-%d", s.prefix, nextSeq)
	} else if n := numericIDSuffix(b.ID); n > nextSeq {
		nextSeq = n
	}
	if b.Status == "" {
		b.Status = "open"
	}
	if b.Type == "" {
		b.Type = "task"
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	return b, nextSeq
}

func (s *BboltStore) findLocked(id string) (Bead, bool) {
	if b, ok := s.main[id]; ok {
		return cloneBead(b), true
	}
	if b, ok := s.wisps[id]; ok {
		return cloneBead(b), true
	}
	return Bead{}, false
}

func (s *BboltStore) upsertOwnedLocked(b Bead) {
	if old, ok := s.main[b.ID]; ok {
		s.mainIdx.remove(old)
		delete(s.main, b.ID)
	}
	if old, ok := s.wisps[b.ID]; ok {
		s.wispIdx.remove(old)
		delete(s.wisps, b.ID)
	}
	if !s.orderSeen[b.ID] {
		s.order = append(s.order, b.ID)
		s.orderSeen[b.ID] = true
	}
	if n := numericIDSuffix(b.ID); n > s.seq {
		s.seq = n
	}
	if b.Ephemeral {
		s.wisps[b.ID] = b
		s.wispIdx.add(b)
		return
	}
	s.main[b.ID] = b
	s.mainIdx.add(b)
}

func (s *BboltStore) deleteLocked(id string) {
	if old, ok := s.main[id]; ok {
		s.mainIdx.remove(old)
		delete(s.main, id)
	}
	if old, ok := s.wisps[id]; ok {
		s.wispIdx.remove(old)
		delete(s.wisps, id)
	}
}

func (s *BboltStore) orderWithIDLocked(id string) []string {
	next := slicesCloneString(s.order)
	if !s.orderSeen[id] {
		next = append(next, id)
	}
	return next
}

func (s *BboltStore) candidateIDsLocked(q ListQuery) hqIDSet {
	switch q.TierMode {
	case TierWisps:
		return s.wispIdx.candidateIDs(q)
	case TierBoth:
		return unionHQIDSets(s.mainIdx.candidateIDs(q), s.wispIdx.candidateIDs(q))
	default:
		return s.mainIdx.candidateIDs(q)
	}
}

func (s *BboltStore) iterationIDsLocked(q ListQuery, candidates hqIDSet) []string {
	if q.Sort == SortDefault {
		return s.order
	}
	ids := make([]string, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	return ids
}

func (s *BboltStore) readyCandidateIDsLocked(q ReadyQuery) []string {
	if q.Assignee == "" {
		return s.order
	}
	ids := make([]string, 0)
	for _, id := range s.order {
		if b, ok := s.main[id]; ok && b.Status == "open" && b.Assignee == q.Assignee {
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *BboltStore) hasOpenChildrenLocked(parentID string) bool {
	for _, child := range s.main {
		if child.ParentID == parentID && child.Status != "closed" && child.Status != "archived" {
			return true
		}
	}
	for _, child := range s.wisps {
		if child.ParentID == parentID && child.Status != "closed" && child.Status != "archived" {
			return true
		}
	}
	return false
}

func (s *BboltStore) maxNumericSuffixLocked() int {
	maxID := 0
	for id := range s.main {
		if n := numericIDSuffix(id); n > maxID {
			maxID = n
		}
	}
	for id := range s.wisps {
		if n := numericIDSuffix(id); n > maxID {
			maxID = n
		}
	}
	return maxID
}

func bboltPutBead(tx *bbolt.Tx, b Bead) error {
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	dst := bboltBucketRecords
	other := bboltBucketWisps
	if b.Ephemeral {
		dst = bboltBucketWisps
		other = bboltBucketRecords
	}
	if err := tx.Bucket(other).Delete([]byte(b.ID)); err != nil {
		return err
	}
	return tx.Bucket(dst).Put([]byte(b.ID), data)
}

func bboltDeleteBead(tx *bbolt.Tx, id string) error {
	if err := tx.Bucket(bboltBucketRecords).Delete([]byte(id)); err != nil {
		return err
	}
	return tx.Bucket(bboltBucketWisps).Delete([]byte(id))
}

func bboltPutSeq(tx *bbolt.Tx, seq int) error {
	return tx.Bucket(bboltBucketMetadata).Put(bboltMetaSeq, bboltEncodeSeq(seq))
}

func bboltPutOrder(tx *bbolt.Tx, order []string) error {
	data, err := json.Marshal(order)
	if err != nil {
		return err
	}
	return tx.Bucket(bboltBucketMetadata).Put(bboltMetaOrder, data)
}

func bboltEncodeSeq(seq int) []byte {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(seq))
	return buf[:]
}

func bboltDecodeSeq(data []byte) int {
	if len(data) != 8 {
		return 0
	}
	return int(binary.LittleEndian.Uint64(data))
}

func bboltDepAdd(in []Dep, issueID, dependsOnID, depType string) []Dep {
	if depType == "" {
		depType = "blocks"
	}
	out := snapshotHQDeps(in)
	for i, d := range out {
		if d.IssueID == issueID && d.DependsOnID == dependsOnID && d.Type == depType {
			return out
		}
		if d.IssueID == issueID && d.DependsOnID == dependsOnID && d.Type != "parent-child" && depType != "parent-child" {
			out[i].Type = depType
			return out
		}
	}
	return append(out, Dep{IssueID: issueID, DependsOnID: dependsOnID, Type: depType})
}

func bboltDepRemove(in []Dep, issueID, dependsOnID string) []Dep {
	out := make([]Dep, 0, len(in))
	for _, d := range in {
		if d.IssueID == issueID && d.DependsOnID == dependsOnID {
			continue
		}
		out = append(out, d)
	}
	return out
}

func bboltDepsWithoutID(in []Dep, id string) []Dep {
	out := make([]Dep, 0, len(in))
	for _, d := range in {
		if d.IssueID == id || d.DependsOnID == id {
			continue
		}
		out = append(out, d)
	}
	return out
}

func bboltPersistChangedDepPairs(tx *bbolt.Tx, before, after []Dep) error {
	pairs := make(map[string]struct{})
	for _, d := range before {
		pairs[bboltDepKey(d.IssueID, d.DependsOnID)] = struct{}{}
	}
	for _, d := range after {
		pairs[bboltDepKey(d.IssueID, d.DependsOnID)] = struct{}{}
	}
	for key := range pairs {
		deps := bboltDepsForKey(after, key)
		if len(deps) == 0 {
			if err := tx.Bucket(bboltBucketDeps).Delete([]byte(key)); err != nil {
				return err
			}
			continue
		}
		data, err := json.Marshal(deps)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bboltBucketDeps).Put([]byte(key), data); err != nil {
			return err
		}
	}
	return nil
}

func bboltDepsForKey(deps []Dep, key string) []Dep {
	var out []Dep
	for _, d := range deps {
		if bboltDepKey(d.IssueID, d.DependsOnID) == key {
			out = append(out, d)
		}
	}
	return out
}

func bboltDecodeDeps(data []byte) ([]Dep, error) {
	var list []Dep
	if err := json.Unmarshal(data, &list); err == nil {
		return list, nil
	}
	var dep Dep
	if err := json.Unmarshal(data, &dep); err != nil {
		return nil, err
	}
	return []Dep{dep}, nil
}

func bboltDepKey(issueID, dependsOnID string) string {
	return issueID + "\x00" + dependsOnID
}
