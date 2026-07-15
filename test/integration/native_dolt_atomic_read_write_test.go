//go:build integration

package integration

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/testutil"
	beadslib "github.com/steveyegge/beads"
)

// TestNativeDoltAtomicReadWriteSerializesIndependentStoreTransactions proves
// the production NativeDolt transaction seam serializes read-modify-write
// callbacks across independent SQL connection pools. The first callback is
// held after its metadata read while the second starts. A correct
// implementation leaves the second visible as a server-side lock waiter and
// then allocates revisions 1 and 2; the old repeatable-read implementation
// enters both callbacks and allocates revision 1 twice.
func TestNativeDoltAtomicReadWriteSerializesIndependentStoreTransactions(t *testing.T) {
	requireDoltIntegration(t)
	env := newIsolatedToolEnv(t, true)

	rootDir := t.TempDir()
	doltDataDir := filepath.Join(rootDir, "dolt")
	workspaceDir := filepath.Join(rootDir, "workspace")
	serverPort := startSharedDoltServer(t, env, doltDataDir)

	beadsDir := filepath.Join(workspaceDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("creating beads directory: %v", err)
	}
	metadata := `{"backend":"dolt","database":"dolt","dolt_mode":"server","dolt_database":"atomic_read_write","dolt_server_host":"127.0.0.1"}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatalf("writing server-mode metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.port"), []byte(serverPort+"\n"), 0o600); err != nil {
		t.Fatalf("writing Dolt server port: %v", err)
	}
	fixtureStorage, err := beadslib.OpenFromConfig(t.Context(), beadsDir)
	if err != nil {
		t.Fatalf("initializing pinned beads schema: %v", err)
	}
	if err := fixtureStorage.SetConfig(t.Context(), "issue_prefix", "arw"); err != nil {
		_ = fixtureStorage.Close()
		t.Fatalf("setting fixture issue prefix: %v", err)
	}
	if err := fixtureStorage.Close(); err != nil {
		t.Fatalf("closing fixture storage: %v", err)
	}

	openEnv := map[string]string{
		"BEADS_DOLT_AUTO_START":  "0",
		"BEADS_DOLT_SERVER_HOST": "127.0.0.1",
		"BEADS_DOLT_SERVER_MODE": "1",
		"BEADS_DOLT_SERVER_PORT": serverPort,
		"BEADS_DOLT_SERVER_USER": "root",
	}
	firstStore, err := beads.OpenNativeDoltStoreAt(t.Context(), workspaceDir, openEnv)
	if err != nil {
		t.Fatalf("opening first NativeDoltStore: %v", err)
	}
	t.Cleanup(func() {
		if err := firstStore.CloseStore(); err != nil {
			t.Errorf("closing first NativeDoltStore: %v", err)
		}
	})
	secondStore, err := beads.OpenNativeDoltStoreAt(t.Context(), workspaceDir, openEnv)
	if err != nil {
		t.Fatalf("opening second NativeDoltStore: %v", err)
	}
	t.Cleanup(func() {
		if err := secondStore.CloseStore(); err != nil {
			t.Errorf("closing second NativeDoltStore: %v", err)
		}
	})

	firstAtomic, ok := beads.AtomicReadWriteFor(firstStore)
	if !ok {
		t.Fatal("first NativeDoltStore has no AtomicReadWrite capability")
	}
	secondAtomic, ok := beads.AtomicReadWriteFor(secondStore)
	if !ok {
		t.Fatal("second NativeDoltStore has no AtomicReadWrite capability")
	}

	observer, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%s)/", serverPort))
	if err != nil {
		t.Fatalf("opening Dolt process-list observer: %v", err)
	}
	t.Cleanup(func() {
		if err := observer.Close(); err != nil {
			t.Errorf("closing Dolt process-list observer: %v", err)
		}
	})

	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	defer func() {
		select {
		case <-releaseFirst:
		default:
			close(releaseFirst)
		}
	}()
	firstErr := make(chan error, 1)
	secondErr := make(chan error, 1)

	go func() {
		firstErr <- allocateNativeDoltRevision(firstAtomic, "arw-command-first", firstEntered, releaseFirst)
	}()
	select {
	case <-firstEntered:
	case err := <-firstErr:
		t.Fatalf("first AtomicReadWrite returned before entering callback: %v", err)
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("first AtomicReadWrite did not enter callback")
	}

	go func() {
		secondErr <- allocateNativeDoltRevision(secondAtomic, "arw-command-second", secondEntered, nil)
	}()

	waitCtx, cancelWait := context.WithTimeout(t.Context(), testutil.GoroutineRaceTimeout)
	secondOverlapped := waitForNativeDoltAtomicContention(t, waitCtx, observer, secondEntered, secondErr)
	cancelWait()
	close(releaseFirst)

	if err := <-firstErr; err != nil {
		t.Fatalf("first AtomicReadWrite: %v", err)
	}
	if err := <-secondErr; err != nil {
		t.Fatalf("second AtomicReadWrite: %v", err)
	}
	if secondOverlapped {
		t.Error("second AtomicReadWrite entered while the first callback was active")
	}

	if err := firstAtomic.AtomicReadWrite(t.Context(), "verify serialized revisions", func(tx beads.AtomicReadWriteTx) error {
		value, err := tx.GetMetadata("arw_revision")
		if err != nil {
			return err
		}
		if value != "2" {
			return fmt.Errorf("arw_revision = %q, want 2", value)
		}
		rows, err := tx.ListHistory(beads.AtomicReadWriteList{IDPrefix: "arw-command-", Limit: 2})
		if err != nil {
			return err
		}
		if len(rows) != 2 {
			return fmt.Errorf("command rows = %d, want 2", len(rows))
		}
		revisions := []string{rows[0].Metadata["revision"], rows[1].Metadata["revision"]}
		sort.Strings(revisions)
		if strings.Join(revisions, ",") != "1,2" {
			return fmt.Errorf("command revisions = %v, want [1 2]", revisions)
		}
		return nil
	}); err != nil {
		t.Fatalf("verifying serialized revisions: %v", err)
	}
}

func allocateNativeDoltRevision(
	store beads.AtomicReadWriteStore,
	commandID string,
	entered chan<- struct{},
	release <-chan struct{},
) error {
	return store.AtomicReadWrite(context.Background(), "allocate NativeDolt revision", func(tx beads.AtomicReadWriteTx) error {
		value, err := tx.GetMetadata("arw_revision")
		if err != nil {
			return err
		}
		var revision uint64
		if value != "" {
			revision, err = strconv.ParseUint(value, 10, 64)
			if err != nil {
				return fmt.Errorf("parsing arw_revision %q: %w", value, err)
			}
		}
		close(entered)
		if release != nil {
			<-release
		}
		revision++
		if _, err := tx.Create(beads.Bead{
			ID: commandID, Title: "NativeDolt atomic command", Type: "task",
			Metadata: map[string]string{"revision": strconv.FormatUint(revision, 10)},
		}); err != nil {
			return err
		}
		return tx.SetMetadata("arw_revision", strconv.FormatUint(revision, 10))
	})
}

// waitForNativeDoltAtomicContention returns true when the second callback
// enters before the first is released. Otherwise it waits until Dolt exposes
// the second connection as a GET_LOCK waiter, proving real server contention.
func waitForNativeDoltAtomicContention(
	t *testing.T,
	ctx context.Context,
	observer *sql.DB,
	secondEntered <-chan struct{},
	secondErr <-chan error,
) bool {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-secondEntered:
			return true
		case err := <-secondErr:
			t.Fatalf("second AtomicReadWrite returned before contention was observed: %v", err)
		case <-ctx.Done():
			t.Fatalf("waiting for second NativeDolt transaction contention: %v", ctx.Err())
		case <-ticker.C:
			waiting, err := doltProcessListContains(t.Context(), observer, "GET_LOCK")
			if err != nil {
				t.Fatalf("observing Dolt process list: %v", err)
			}
			if waiting {
				return false
			}
		}
	}
}

func doltProcessListContains(ctx context.Context, db *sql.DB, needle string) (bool, error) {
	rows, err := db.QueryContext(ctx, "SHOW FULL PROCESSLIST")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return false, err
	}
	infoIndex := -1
	for i, column := range columns {
		if strings.EqualFold(column, "Info") {
			infoIndex = i
			break
		}
	}
	if infoIndex < 0 {
		return false, fmt.Errorf("SHOW FULL PROCESSLIST has no Info column: %v", columns)
	}
	for rows.Next() {
		values := make([]sql.NullString, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return false, err
		}
		if values[infoIndex].Valid && strings.Contains(strings.ToUpper(values[infoIndex].String), needle) {
			return true, nil
		}
	}
	return false, rows.Err()
}
