package main

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

func TestOpenProductionNudgeCommandSourceProvisionsButRefusesUnverifiedCityPartition(t *testing.T) {
	store := newNudgeCommandSourceAtomicStore()
	cityPath := t.TempDir()

	first, err := openVerifiedProductionNudgeCommandSource(t.Context(), cityPath, store, nudgequeue.TrustedCityPartition{}, nil)
	if first != nil || !errors.Is(err, errNudgeCommandSourceUnverified) || !errors.Is(err, nudgequeue.ErrCommandRepositoryPartition) {
		t.Fatalf("first open = %T, err=%v; want unverified partition refusal", first, err)
	}
	if writes := store.metadataWriteCount(); writes != 6 {
		t.Fatalf("initial metadata writes = %d, want 6", writes)
	}
	if _, exists, err := nudgequeue.LoadRestoreAnchor(t.Context(), nudgequeue.RestoreAnchorPath(cityPath)); err != nil || !exists {
		t.Fatalf("independent restore anchor after first open: exists=%t err=%v", exists, err)
	}

	second, err := openVerifiedProductionNudgeCommandSource(t.Context(), cityPath, store, nudgequeue.TrustedCityPartition{}, nil)
	if second != nil || !errors.Is(err, errNudgeCommandSourceUnverified) || !errors.Is(err, nudgequeue.ErrCommandRepositoryPartition) {
		t.Fatalf("second open = %T, err=%v; want stable unverified partition refusal", second, err)
	}
	if writes := store.metadataWriteCount(); writes != 6 {
		t.Fatalf("unverified reopen metadata writes = %d, want 6", writes)
	}
}

func TestOpenProductionNudgeCommandSourceLeavesUnsupportedStoreLegacyOnly(t *testing.T) {
	source, err := openVerifiedProductionNudgeCommandSource(t.Context(), t.TempDir(), beads.NewMemStore(), nudgequeue.TrustedCityPartition{}, nil)
	if source != nil {
		t.Fatalf("unsupported source = %T, want nil", source)
	}
	if !errors.Is(err, errNudgeCommandSourceUnverified) || !errors.Is(err, nudgequeue.ErrCommandRepositoryUnsupported) {
		t.Fatalf("unsupported error = %v, want unverified + repository unsupported", err)
	}
}

func TestOpenProductionNudgeCommandSourceWrapsKnownTransientProvisionFailure(t *testing.T) {
	store := newNudgeCommandSourceAtomicStore()
	store.failNext = context.DeadlineExceeded

	source, err := openVerifiedProductionNudgeCommandSource(t.Context(), t.TempDir(), store, nudgequeue.TrustedCityPartition{}, nil)
	if source != nil {
		t.Fatalf("transient source = %T, want nil until retry", source)
	}
	var failure nudgeCommandSourceFailure
	if !errors.As(err, &failure) || failure.class != nudgeCommandSourceErrorTransient || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("transient open error = %#v (%v), want retryable deadline failure", failure, err)
	}
}

func TestProductionNudgeCommandSourceCanHoldOnlyPartitionedRepositoryReader(t *testing.T) {
	typ := reflect.TypeOf(productionNudgeCommandSource{})
	field, ok := typ.FieldByName("repository")
	if !ok || field.Type != reflect.TypeOf((*nudgequeue.CommandPartitionReader)(nil)) {
		t.Fatalf("production source repository field = %#v, want *nudgequeue.CommandPartitionReader", field)
	}
}

func TestProductionNudgeCommandSourceClassifiesOnlyKnownRetryableFailures(t *testing.T) {
	source := &productionNudgeCommandSource{}
	for _, err := range []error{context.DeadlineExceeded, nudgequeue.ErrRestoreAnchorBusy, nudgequeue.ErrRestoreAnchorConflict, nudgequeue.ErrRestoreAnchorDurabilityUncertain} {
		if got := source.ClassifyNudgeCommandSourceError(err); got != nudgeCommandSourceErrorTransient {
			t.Errorf("ClassifyNudgeCommandSourceError(%v) = %d, want transient", err, got)
		}
	}
	for _, err := range []error{errors.New("unknown"), nudgequeue.ErrCommandRepositoryLineage, nudgequeue.ErrCommandRepositorySchemaSkew, nudgequeue.ErrCommandRepositoryRecord} {
		if got := source.ClassifyNudgeCommandSourceError(err); got != nudgeCommandSourceErrorInvariant {
			t.Errorf("ClassifyNudgeCommandSourceError(%v) = %d, want invariant", err, got)
		}
	}
}

type nudgeCommandSourceAtomicStore struct {
	beads.Store

	mu             sync.Mutex
	metadata       map[string]string
	rows           map[string]beads.Bead
	metadataWrites int
	failNext       error
}

func newNudgeCommandSourceAtomicStore() *nudgeCommandSourceAtomicStore {
	return &nudgeCommandSourceAtomicStore{
		Store:    beads.NewMemStore(),
		metadata: make(map[string]string),
		rows:     make(map[string]beads.Bead),
	}
}

func (s *nudgeCommandSourceAtomicStore) AtomicReadWrite(ctx context.Context, _ string, fn func(beads.AtomicReadWriteTx) error) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext != nil {
		err := s.failNext
		s.failNext = nil
		return err
	}
	tx := &nudgeCommandSourceAtomicTx{
		metadata:       cloneNudgeCommandSourceStrings(s.metadata),
		rows:           cloneNudgeCommandSourceRows(s.rows),
		metadataWrites: s.metadataWrites,
	}
	if err := fn(tx); err != nil {
		return err
	}
	s.metadata = tx.metadata
	s.rows = tx.rows
	s.metadataWrites = tx.metadataWrites
	return nil
}

func (s *nudgeCommandSourceAtomicStore) metadataWriteCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metadataWrites
}

type nudgeCommandSourceAtomicTx struct {
	metadata       map[string]string
	rows           map[string]beads.Bead
	metadataWrites int
}

func (tx *nudgeCommandSourceAtomicTx) GetIssue(id string) (beads.Bead, error) {
	row, ok := tx.rows[id]
	if !ok {
		return beads.Bead{}, beads.ErrNotFound
	}
	return cloneNudgeCommandSourceRow(row), nil
}

func (tx *nudgeCommandSourceAtomicTx) ListHistory(query beads.AtomicReadWriteList) ([]beads.Bead, error) {
	ids := make(map[string]struct{}, len(query.IDs))
	for _, id := range query.IDs {
		ids[id] = struct{}{}
	}
	var rows []beads.Bead
	for _, row := range tx.rows {
		if len(ids) > 0 {
			if _, ok := ids[row.ID]; !ok {
				continue
			}
		}
		if query.IDPrefix != "" && !strings.HasPrefix(row.ID, query.IDPrefix) {
			continue
		}
		if query.IssueType != "" && row.Type != query.IssueType {
			continue
		}
		matches := true
		for key, value := range query.Metadata {
			if row.Metadata[key] != value {
				matches = false
				break
			}
		}
		if matches {
			rows = append(rows, cloneNudgeCommandSourceRow(row))
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	if len(rows) > query.Limit {
		rows = rows[:query.Limit]
	}
	return rows, nil
}

func (tx *nudgeCommandSourceAtomicTx) Create(row beads.Bead) (beads.Bead, error) {
	if _, exists := tx.rows[row.ID]; exists {
		return beads.Bead{}, errors.New("duplicate row")
	}
	tx.rows[row.ID] = cloneNudgeCommandSourceRow(row)
	return cloneNudgeCommandSourceRow(row), nil
}

func (tx *nudgeCommandSourceAtomicTx) Update(id string, opts beads.UpdateOpts) error {
	row, ok := tx.rows[id]
	if !ok {
		return beads.ErrNotFound
	}
	if opts.Status != nil {
		row.Status = *opts.Status
	}
	if opts.Metadata != nil {
		if row.Metadata == nil {
			row.Metadata = make(map[string]string)
		}
		for key, value := range opts.Metadata {
			row.Metadata[key] = value
		}
	}
	tx.rows[id] = row
	return nil
}

func (tx *nudgeCommandSourceAtomicTx) GetMetadata(key string) (string, error) {
	return tx.metadata[key], nil
}

func (tx *nudgeCommandSourceAtomicTx) SetMetadata(key, value string) error {
	tx.metadata[key] = value
	tx.metadataWrites++
	return nil
}

func cloneNudgeCommandSourceStrings(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneNudgeCommandSourceRows(source map[string]beads.Bead) map[string]beads.Bead {
	result := make(map[string]beads.Bead, len(source))
	for id, row := range source {
		result[id] = cloneNudgeCommandSourceRow(row)
	}
	return result
}

func cloneNudgeCommandSourceRow(row beads.Bead) beads.Bead {
	row.Metadata = cloneNudgeCommandSourceStrings(row.Metadata)
	return row
}
