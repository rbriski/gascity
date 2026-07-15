package beads

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const nativeDoltStatusIDSnapshotIndex = "gc_idx_issues_status_id"

// PrepareAtomicReadSnapshot installs the Gas City-owned `(status,id)`
// companion index used for bounded current-state and partition-prefix reads.
// It is deliberately separate from AtomicReadSnapshot so read paths remain
// side-effect free. An index with the owned name but different columns is
// schema skew and fails closed.
func (s *NativeDoltStore) PrepareAtomicReadSnapshot(parent context.Context) error {
	if parent == nil {
		return errors.New("preparing beads atomic read snapshot: nil context")
	}
	if err := parent.Err(); err != nil {
		return err
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(parent)
	defer cancel()
	db, cleanup, err := openNativeDoltSnapshotDB(ctx, storage)
	if err != nil {
		return err
	}
	defer func() { _ = cleanup() }()
	columns, present, err := nativeDoltSnapshotIndexColumns(ctx, db, nativeDoltStatusIDSnapshotIndex)
	if err != nil {
		return fmt.Errorf("checking NativeDolt atomic snapshot paging index %q: %w", nativeDoltStatusIDSnapshotIndex, errors.Join(ErrAtomicReadSnapshotUnsupported, err))
	}
	if present {
		if columns != "status,id" {
			return fmt.Errorf("NativeDolt atomic snapshot paging index %q columns = %q, want status,id: %w", nativeDoltStatusIDSnapshotIndex, columns, ErrAtomicReadSnapshotUnsupported)
		}
		return nil
	}
	if _, err := db.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS "+nativeDoltStatusIDSnapshotIndex+" ON issues (status, id)"); err != nil {
		return fmt.Errorf("installing NativeDolt atomic snapshot paging index %q: %w", nativeDoltStatusIDSnapshotIndex, errors.Join(ErrAtomicReadSnapshotUnsupported, err))
	}
	columns, present, err = nativeDoltSnapshotIndexColumns(ctx, db, nativeDoltStatusIDSnapshotIndex)
	if err != nil {
		return fmt.Errorf("verifying installed NativeDolt atomic snapshot paging index %q: %w", nativeDoltStatusIDSnapshotIndex, errors.Join(ErrAtomicReadSnapshotUnsupported, err))
	}
	if !present || columns != "status,id" {
		return fmt.Errorf("installed NativeDolt atomic snapshot paging index %q columns = %q, want status,id: %w", nativeDoltStatusIDSnapshotIndex, columns, ErrAtomicReadSnapshotUnsupported)
	}
	return nil
}

// AtomicReadSnapshot holds one repeatable-read SQL snapshot across every exact
// metadata/record read and bounded keyset page in fn. The callback surface is
// read-only by construction. NativeDolt's upstream transaction SearchIssues
// currently ignores its Offset field, so this capability uses the raw provider
// boundary and an explicitly verified standard-column index.
func (s *NativeDoltStore) AtomicReadSnapshot(parent context.Context, fn func(AtomicReadSnapshotTx) error) error {
	if parent == nil {
		return errors.New("beads atomic read snapshot: nil context")
	}
	if err := parent.Err(); err != nil {
		return err
	}
	if fn == nil {
		return errors.New("beads atomic read snapshot: nil callback")
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(parent)
	defer cancel()
	db, cleanup, err := openNativeDoltSnapshotDB(ctx, storage)
	if err != nil {
		return err
	}
	defer func() { _ = cleanup() }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("pinning NativeDolt atomic read snapshot connection: %w", errors.Join(ErrAtomicReadSnapshotUnsupported, err))
	}
	defer func() { _ = conn.Close() }()
	var isolation string
	if err := conn.QueryRowContext(ctx, "SELECT @@transaction_isolation").Scan(&isolation); err != nil {
		return fmt.Errorf("verifying NativeDolt atomic read snapshot isolation: %w", errors.Join(ErrAtomicReadSnapshotUnsupported, err))
	}
	if canonicalIsolation := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(isolation), "_", "-")); canonicalIsolation != "REPEATABLE-READ" {
		return fmt.Errorf("NativeDolt atomic read snapshot isolation = %q, want REPEATABLE-READ: %w", isolation, ErrAtomicReadSnapshotUnsupported)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning NativeDolt atomic read snapshot: %w", errors.Join(ErrAtomicReadSnapshotUnsupported, err))
	}
	defer func() { _ = tx.Rollback() }()
	snapshot := &nativeDoltAtomicReadSnapshotTx{ctx: ctx, tx: tx}
	if err := snapshot.verifyPagingIndexes(); err != nil {
		return err
	}
	if err := fn(snapshot); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing NativeDolt atomic read snapshot: %w", err)
	}
	return nil
}

type nativeDoltAtomicReadSnapshotTx struct {
	ctx context.Context
	tx  *sql.Tx
}

type nativeDoltSnapshotIndexQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func nativeDoltSnapshotIndexColumns(ctx context.Context, queryer nativeDoltSnapshotIndexQueryer, indexName string) (string, bool, error) {
	var columns sql.NullString
	err := queryer.QueryRowContext(ctx, `
		SELECT GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX SEPARATOR ',')
		FROM INFORMATION_SCHEMA.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE()
		  AND TABLE_NAME = 'issues'
		  AND INDEX_NAME = ?
	`, indexName).Scan(&columns)
	if err != nil {
		return "", false, err
	}
	return columns.String, columns.Valid, nil
}

func (t *nativeDoltAtomicReadSnapshotTx) verifyPagingIndexes() error {
	indexes := []struct {
		name    string
		columns string
	}{
		{name: "idx_issues_status_updated_at", columns: "status,updated_at"},
		{name: nativeDoltStatusIDSnapshotIndex, columns: "status,id"},
	}
	for _, index := range indexes {
		columns, present, err := nativeDoltSnapshotIndexColumns(t.ctx, t.tx, index.name)
		if err != nil {
			return fmt.Errorf("verifying NativeDolt atomic snapshot paging index %q: %w", index.name, errors.Join(ErrAtomicReadSnapshotUnsupported, err))
		}
		if !present || columns != index.columns {
			return fmt.Errorf("NativeDolt atomic snapshot paging index %q columns = %q, want %s: %w", index.name, columns, index.columns, ErrAtomicReadSnapshotUnsupported)
		}
	}
	return nil
}

func (t *nativeDoltAtomicReadSnapshotTx) GetIssue(id string) (Bead, error) {
	if strings.TrimSpace(id) == "" {
		return Bead{}, fmt.Errorf("snapshot exact id is empty: %w", ErrAtomicReadSnapshotQuery)
	}
	row := t.tx.QueryRowContext(t.ctx, `
		SELECT id, title, status, issue_type, created_at, updated_at, metadata,
		       COALESCE(ephemeral, 0), COALESCE(no_history, 0)
		FROM issues
		WHERE id = ?
	`, id)
	bead, err := scanNativeDoltAtomicSnapshotBead(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Bead{}, fmt.Errorf("bead %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Bead{}, fmt.Errorf("getting exact history row %q in atomic snapshot: %w", id, err)
	}
	if err := requireAtomicReadWriteHistory(bead); err != nil {
		return Bead{}, err
	}
	return bead, nil
}

func (t *nativeDoltAtomicReadSnapshotTx) ListHistoryPage(query AtomicReadSnapshotPageQuery) (page AtomicReadSnapshotPage, err error) {
	if err := validateAtomicReadSnapshotPageQuery(query); err != nil {
		return AtomicReadSnapshotPage{}, err
	}
	args := []any{query.Status, query.IDPrefix + "%"}
	var indexName, keysetSQL, orderSQL string
	switch query.Order {
	case AtomicReadSnapshotOrderID:
		indexName = nativeDoltStatusIDSnapshotIndex
		orderSQL = "id ASC"
		if query.After != (AtomicReadSnapshotCursor{}) {
			keysetSQL = "AND id > ?"
			args = append(args, query.After.ID)
		}
	case AtomicReadSnapshotOrderUpdatedAtID:
		indexName = "idx_issues_status_updated_at"
		orderSQL = "updated_at ASC, id ASC"
		if query.After != (AtomicReadSnapshotCursor{}) {
			keysetSQL = "AND (updated_at, id) > (?, ?)"
			args = append(args, query.After.UpdatedAt, query.After.ID)
		}
	default:
		return AtomicReadSnapshotPage{}, fmt.Errorf("unsupported NativeDolt snapshot order %d: %w", query.Order, ErrAtomicReadSnapshotQuery)
	}
	args = append(args, query.Limit)
	// IDPrefix rejects LIKE metacharacters, and every value remains bound. The
	// forced index makes absence/skew fail loudly instead of degrading into a
	// lifetime-sized scan.
	querySQL := fmt.Sprintf(`
		SELECT id, title, status, issue_type, created_at, updated_at, metadata,
		       COALESCE(ephemeral, 0), COALESCE(no_history, 0)
		FROM issues FORCE INDEX (%s)
		WHERE status = ? AND id LIKE ? %s
		ORDER BY %s
		LIMIT ?
	`, indexName, keysetSQL, orderSQL)
	rows, err := t.tx.QueryContext(t.ctx, querySQL, args...)
	if err != nil {
		return AtomicReadSnapshotPage{}, fmt.Errorf("listing NativeDolt atomic snapshot page: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("closing NativeDolt atomic snapshot page: %w", closeErr))
		}
	}()
	page = AtomicReadSnapshotPage{Rows: make([]Bead, 0, query.Limit)}
	for rows.Next() {
		bead, err := scanNativeDoltAtomicSnapshotBead(rows)
		if err != nil {
			return AtomicReadSnapshotPage{}, fmt.Errorf("scanning NativeDolt atomic snapshot page: %w", err)
		}
		page.Rows = append(page.Rows, bead)
	}
	if err := rows.Err(); err != nil {
		return AtomicReadSnapshotPage{}, fmt.Errorf("iterating NativeDolt atomic snapshot page: %w", err)
	}
	if len(page.Rows) == query.Limit {
		page.Next = atomicReadSnapshotCursorForRow(query.Order, page.Rows[len(page.Rows)-1])
	}
	if err := validateAtomicReadSnapshotPage(query, page); err != nil {
		return AtomicReadSnapshotPage{}, err
	}
	return page, nil
}

func (t *nativeDoltAtomicReadSnapshotTx) GetMetadata(key string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("snapshot metadata key is empty: %w", ErrAtomicReadSnapshotQuery)
	}
	var value string
	err := t.tx.QueryRowContext(t.ctx, "SELECT value FROM metadata WHERE `key` = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("getting durable snapshot metadata %q: %w", key, err)
	}
	return value, nil
}

type nativeDoltAtomicSnapshotScanner interface {
	Scan(...any) error
}

func scanNativeDoltAtomicSnapshotBead(scanner nativeDoltAtomicSnapshotScanner) (Bead, error) {
	var (
		bead      Bead
		metadata  []byte
		ephemeral bool
		noHistory bool
	)
	if err := scanner.Scan(
		&bead.ID,
		&bead.Title,
		&bead.Status,
		&bead.Type,
		&bead.CreatedAt,
		&bead.UpdatedAt,
		&metadata,
		&ephemeral,
		&noHistory,
	); err != nil {
		return Bead{}, err
	}
	bead.CreatedAt = bead.CreatedAt.UTC()
	bead.UpdatedAt = bead.UpdatedAt.UTC()
	bead.Ephemeral = ephemeral
	bead.NoHistory = noHistory
	if len(metadata) > 0 && string(metadata) != "null" {
		if err := json.Unmarshal(metadata, &bead.Metadata); err != nil {
			return Bead{}, fmt.Errorf("decoding history row %q metadata: %w", bead.ID, err)
		}
	}
	if bead.Metadata == nil {
		bead.Metadata = make(StringMap)
	}
	return bead, nil
}
