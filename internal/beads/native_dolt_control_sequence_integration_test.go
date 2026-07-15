//go:build integration

package beads

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	beadslib "github.com/steveyegge/beads"
)

func TestNativeDoltControlSequenceProjectionRejectsExactSchemaSkew(t *testing.T) {
	for _, skew := range []string{"generated expression", "index columns", "retired index shape"} {
		t.Run(skew, func(t *testing.T) {
			store, storage := newNativeDoltControlSequenceTestStore(t)
			ctx := t.Context()
			db, cleanup, err := openNativeDoltSnapshotDB(ctx, storage)
			if err != nil {
				t.Fatalf("open snapshot database for control-sequence skew: %v", err)
			}
			switch skew {
			case "generated expression":
				if _, err := db.ExecContext(ctx, "DROP INDEX "+nativeDoltControlSequenceSnapshotIndex+" ON issues"); err != nil {
					t.Fatalf("drop control-sequence index: %v", err)
				}
				if _, err := db.ExecContext(ctx, "ALTER TABLE issues DROP COLUMN "+nativeDoltControlSequenceColumn); err != nil {
					t.Fatalf("drop control-sequence projection: %v", err)
				}
				wrongExpression := fmt.Sprintf(`ALTER TABLE issues ADD COLUMN %s BIGINT UNSIGNED GENERATED ALWAYS AS (
					CAST(JSON_UNQUOTE(JSON_EXTRACT(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$."%s"')), '$.order.generation')) AS UNSIGNED)
				) STORED`, nativeDoltControlSequenceColumn, beadmeta.ControlCommandWireMetadataKey)
				if _, err := db.ExecContext(ctx, wrongExpression); err != nil {
					t.Fatalf("install skewed control-sequence projection: %v", err)
				}
				if _, err := db.ExecContext(ctx, "CREATE INDEX "+nativeDoltControlSequenceSnapshotIndex+" ON issues ("+nativeDoltControlSequenceColumn+",id)"); err != nil {
					t.Fatalf("restore control-sequence index over skewed projection: %v", err)
				}
			case "index columns":
				if _, err := db.ExecContext(ctx, "DROP INDEX "+nativeDoltControlSequenceSnapshotIndex+" ON issues"); err != nil {
					t.Fatalf("drop control-sequence index: %v", err)
				}
				if _, err := db.ExecContext(ctx, "CREATE INDEX "+nativeDoltControlSequenceSnapshotIndex+" ON issues (id,"+nativeDoltControlSequenceColumn+")"); err != nil {
					t.Fatalf("install skewed control-sequence index: %v", err)
				}
			case "retired index shape":
				_, _, present, err := nativeDoltSnapshotIndexDefinition(ctx, db, nativeDoltLegacyControlSequenceUniqueIndex)
				if err != nil {
					t.Fatalf("inspect retired control-sequence index: %v", err)
				}
				if present {
					if _, err := db.ExecContext(ctx, "DROP INDEX "+nativeDoltLegacyControlSequenceUniqueIndex+" ON issues"); err != nil {
						t.Fatalf("drop retired control-sequence index: %v", err)
					}
				}
				if _, err := db.ExecContext(ctx, "CREATE INDEX "+nativeDoltLegacyControlSequenceUniqueIndex+" ON issues ("+nativeDoltControlSequenceColumn+")"); err != nil {
					t.Fatalf("install malformed retired control-sequence index: %v", err)
				}
			default:
				t.Fatalf("unknown skew %q", skew)
			}
			if err := cleanup(); err != nil {
				t.Fatalf("close snapshot database after control-sequence skew: %v", err)
			}

			if err := store.PrepareAtomicReadSnapshot(ctx); !errors.Is(err, ErrAtomicReadSnapshotUnsupported) {
				t.Fatalf("PrepareAtomicReadSnapshot error = %v, want ErrAtomicReadSnapshotUnsupported", err)
			}
			called := false
			err = store.AtomicReadSnapshot(ctx, func(AtomicReadSnapshotTx) error {
				called = true
				return nil
			})
			if !errors.Is(err, ErrAtomicReadSnapshotUnsupported) {
				t.Fatalf("AtomicReadSnapshot error = %v, want ErrAtomicReadSnapshotUnsupported", err)
			}
			if called {
				t.Fatal("AtomicReadSnapshot called callback with skewed control-sequence schema")
			}
		})
	}
}

func TestNativeDoltControlSequenceLookupDistinguishesMissingAndUnique(t *testing.T) {
	store, _ := newNativeDoltControlSequenceTestStore(t)
	createNativeDoltControlSequenceTestBead(t, store, "gc-command-a", 7)
	createNativeDoltControlSequenceTestBead(t, store, "gc-command-c", 8)

	err := store.AtomicReadSnapshot(t.Context(), func(tx AtomicReadSnapshotTx) error {
		missing, err := tx.ListHistoryByControlSequence(AtomicReadSnapshotControlSequenceQuery{
			IDPrefix: "gc-command-",
			Sequence: 6,
			Limit:    2,
		})
		if err != nil {
			return fmt.Errorf("lookup missing sequence: %w", err)
		}
		if len(missing.Rows) != 0 {
			return fmt.Errorf("missing sequence rows = %#v, want none", missing.Rows)
		}

		unique, err := tx.ListHistoryByControlSequence(AtomicReadSnapshotControlSequenceQuery{
			IDPrefix: "gc-command-",
			Sequence: 7,
			Limit:    2,
		})
		if err != nil {
			return fmt.Errorf("lookup duplicate sequence: %w", err)
		}
		if len(unique.Rows) != 1 || unique.Rows[0].ID != "gc-command-a" {
			return fmt.Errorf("unique sequence rows = %#v, want gc-command-a", unique.Rows)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("AtomicReadSnapshot exact control-sequence lookups: %v", err)
	}
}

func TestNativeDoltControlSequenceLegacyUniqueIndexIsRepairedWithoutLosingRows(t *testing.T) {
	store, storage := newNativeDoltControlSequenceTestStore(t)
	createNativeDoltControlSequenceTestBead(t, store, "gc-command-a", 7)
	createNativeDoltControlSequenceTestBead(t, store, "gc-command-b", 8)

	ctx := t.Context()
	db, cleanup, err := openNativeDoltSnapshotDB(ctx, storage)
	if err != nil {
		t.Fatalf("open snapshot database for legacy control-sequence index: %v", err)
	}
	columns, unique, present, err := nativeDoltSnapshotIndexDefinition(ctx, db, nativeDoltLegacyControlSequenceUniqueIndex)
	if err != nil {
		t.Fatalf("inspect legacy control-sequence index: %v", err)
	}
	if !present {
		if _, err := db.ExecContext(ctx, "CREATE UNIQUE INDEX "+nativeDoltLegacyControlSequenceUniqueIndex+" ON issues ("+nativeDoltControlSequenceColumn+")"); err != nil {
			t.Fatalf("install legacy unique control-sequence index: %v", err)
		}
	} else if columns != nativeDoltControlSequenceColumn || !unique {
		t.Fatalf("legacy control-sequence index definition = columns:%q unique:%t", columns, unique)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("close snapshot database after legacy index installation: %v", err)
	}

	if err := store.PrepareAtomicReadSnapshot(ctx); err != nil {
		t.Fatalf("repair legacy unique control-sequence index: %v", err)
	}
	db, cleanup, err = openNativeDoltSnapshotDB(ctx, storage)
	if err != nil {
		t.Fatalf("reopen snapshot database after legacy repair: %v", err)
	}
	_, _, present, err = nativeDoltSnapshotIndexDefinition(ctx, db, nativeDoltLegacyControlSequenceUniqueIndex)
	if err != nil {
		t.Fatalf("inspect retired control-sequence index after repair: %v", err)
	}
	if present {
		t.Fatal("retired unique control-sequence index remains after repair")
	}
	if err := cleanup(); err != nil {
		t.Fatalf("close snapshot database after legacy repair verification: %v", err)
	}

	if err := store.Close("gc-command-a"); err != nil {
		t.Fatalf("terminalize row after legacy index repair: %v", err)
	}
	got, err := store.Get("gc-command-a")
	if err != nil {
		t.Fatalf("read terminal row after legacy index repair: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("terminal row status = %q, want closed", got.Status)
	}
	err = store.AtomicReadSnapshot(ctx, func(tx AtomicReadSnapshotTx) error {
		page, err := tx.ListHistoryByControlSequence(AtomicReadSnapshotControlSequenceQuery{
			IDPrefix: "gc-command-", Sequence: 8, Limit: 2,
		})
		if err != nil {
			return err
		}
		if len(page.Rows) != 1 || page.Rows[0].ID != "gc-command-b" {
			return fmt.Errorf("sequence 8 rows = %#v, want gc-command-b", page.Rows)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("exact lookup after legacy index repair: %v", err)
	}
}

func TestNativeDoltControlSequenceLookupReturnsDuplicateRawProjections(t *testing.T) {
	store, _ := newNativeDoltControlSequenceTestStore(t)
	createNativeDoltControlSequenceTestBead(t, store, "gc-command-a", 7)
	createNativeDoltControlSequenceTestBead(t, store, "gc-command-b", 7)

	err := store.AtomicReadSnapshot(t.Context(), func(tx AtomicReadSnapshotTx) error {
		page, err := tx.ListHistoryByControlSequence(AtomicReadSnapshotControlSequenceQuery{
			IDPrefix: "gc-command-", Sequence: 7, Limit: 2,
		})
		if err != nil {
			return err
		}
		if len(page.Rows) != 2 || page.Rows[0].ID != "gc-command-a" || page.Rows[1].ID != "gc-command-b" {
			return fmt.Errorf("duplicate sequence rows = %#v, want [gc-command-a gc-command-b]", page.Rows)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("AtomicReadSnapshot duplicate control-sequence lookup: %v", err)
	}
}

func TestNativeDoltControlSequenceLookupSupportsMaxUint64(t *testing.T) {
	store, _ := newNativeDoltControlSequenceTestStore(t)
	maxSequence := ^uint64(0)
	createNativeDoltControlSequenceTestBead(t, store, "gc-command-max", maxSequence)

	err := store.AtomicReadSnapshot(t.Context(), func(tx AtomicReadSnapshotTx) error {
		page, err := tx.ListHistoryByControlSequence(AtomicReadSnapshotControlSequenceQuery{
			IDPrefix: "gc-command-",
			Sequence: maxSequence,
			Limit:    2,
		})
		if err != nil {
			return err
		}
		if len(page.Rows) != 1 || page.Rows[0].ID != "gc-command-max" {
			return fmt.Errorf("max-uint64 sequence rows = %#v, want gc-command-max", page.Rows)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("AtomicReadSnapshot max-uint64 control sequence: %v", err)
	}
}

func TestNativeDoltControlSequenceLookupPlanUsesForcedOrderedIndex(t *testing.T) {
	_, storage := newNativeDoltControlSequenceTestStore(t)
	ctx := t.Context()
	db, cleanup, err := openNativeDoltSnapshotDB(ctx, storage)
	if err != nil {
		t.Fatalf("open snapshot database for control-sequence plan: %v", err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Fatalf("close snapshot database after control-sequence plan: %v", err)
		}
	}()

	columns, _, present, err := nativeDoltSnapshotIndexDefinition(ctx, db, nativeDoltControlSequenceSnapshotIndex)
	if err != nil {
		t.Fatalf("read control-sequence index: %v", err)
	}
	if !present || columns != nativeDoltControlSequenceColumn+",id" {
		t.Fatalf("control-sequence index present/columns = %t/%q, want true/%s,id", present, columns, nativeDoltControlSequenceColumn)
	}

	explainRows, err := db.QueryContext(ctx, `
		EXPLAIN FORMAT=TREE
		SELECT id
		FROM issues FORCE INDEX (`+nativeDoltControlSequenceSnapshotIndex+`)
		WHERE `+nativeDoltControlSequenceColumn+` = ? AND id LIKE ?
		ORDER BY `+nativeDoltControlSequenceColumn+` ASC, id ASC
		LIMIT ?
	`, "7", "gc-command-%", 2)
	if err != nil {
		t.Fatalf("explain control-sequence snapshot query: %v", err)
	}
	var plan strings.Builder
	for explainRows.Next() {
		var line string
		if err := explainRows.Scan(&line); err != nil {
			_ = explainRows.Close()
			t.Fatalf("scan control-sequence snapshot plan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := explainRows.Err(); err != nil {
		_ = explainRows.Close()
		t.Fatalf("iterate control-sequence snapshot plan: %v", err)
	}
	if err := explainRows.Close(); err != nil {
		t.Fatalf("close control-sequence snapshot plan: %v", err)
	}
	planText := plan.String()
	if !strings.Contains(planText, "[issues."+nativeDoltControlSequenceColumn+",issues.id]") {
		t.Fatalf("control-sequence snapshot plan does not prove the owned index columns:\n%s", planText)
	}
	for _, forbidden := range []string{"TableScan", "Sort("} {
		if strings.Contains(planText, forbidden) {
			t.Fatalf("control-sequence snapshot plan contains unbounded %s operator:\n%s", forbidden, planText)
		}
	}
}

func newNativeDoltControlSequenceTestStore(t *testing.T) (*NativeDoltStore, beadslib.Storage) {
	t.Helper()
	ctx := t.Context()
	storage, err := beadslib.OpenBestAvailable(ctx, filepath.Join(t.TempDir(), ".beads"))
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Fatalf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "control-sequence-test", "gc")
	if err := store.PrepareAtomicReadSnapshot(ctx); err != nil {
		t.Fatalf("PrepareAtomicReadSnapshot: %v", err)
	}
	return store, storage
}

func createNativeDoltControlSequenceTestBead(t *testing.T, store *NativeDoltStore, id string, sequence uint64) {
	t.Helper()
	wire := fmt.Sprintf(`{"version":1,"order":{"sequence":%d}}`, sequence)
	if _, err := store.Create(Bead{
		ID:    id,
		Title: id,
		Metadata: map[string]string{
			beadmeta.ControlCommandWireMetadataKey: wire,
		},
	}); err != nil {
		t.Fatalf("Create control-command bead %s: %v", id, err)
	}
}
