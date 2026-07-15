package nudgequeue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteRestoreAnchorCrashWindows(t *testing.T) {
	injected := errors.New("injected filesystem failure")
	tests := []struct {
		name          string
		mutateOps     func(*restoreAnchorFileOps)
		wantPublished bool
		wantUncertain bool
	}{
		{
			name: "file sync fails before publication",
			mutateOps: func(ops *restoreAnchorFileOps) {
				ops.syncFile = func(*os.File) error { return injected }
			},
		},
		{
			name: "rename reports failure before publication",
			mutateOps: func(ops *restoreAnchorFileOps) {
				ops.rename = func(string, string) error { return injected }
			},
			wantUncertain: true,
		},
		{
			name: "rename publishes then loses acknowledgement",
			mutateOps: func(ops *restoreAnchorFileOps) {
				ops.rename = func(oldPath, newPath string) error {
					if err := os.Rename(oldPath, newPath); err != nil {
						return err
					}
					return injected
				}
			},
			wantPublished: true,
			wantUncertain: true,
		},
		{
			name: "parent sync fails after publication",
			mutateOps: func(ops *restoreAnchorFileOps) {
				ops.syncDirectory = func(string) error { return injected }
			},
			wantPublished: true,
			wantUncertain: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := RestoreAnchorPath(t.TempDir())
			current := RestoreAnchor{
				Version:                 RestoreAnchorVersion1,
				Store:                   CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: 1},
				HighestAcceptedRevision: 3,
				HighestAcceptedSequence: 2,
			}
			if err := WriteRestoreAnchor(context.Background(), path, nil, current, RestoreAnchorWriteInitialize); err != nil {
				t.Fatalf("initialize: %v", err)
			}
			next := current
			next.HighestAcceptedRevision++
			ops := osRestoreAnchorFileOps
			tc.mutateOps(&ops)
			err := writeRestoreAnchor(context.Background(), path, &current, next, RestoreAnchorWriteAdvance, ops)
			if !errors.Is(err, injected) {
				t.Fatalf("writeRestoreAnchor error = %v, want injected error", err)
			}
			if got := errors.Is(err, ErrRestoreAnchorDurabilityUncertain); got != tc.wantUncertain {
				t.Errorf("durability uncertain = %t, want %t (error %v)", got, tc.wantUncertain, err)
			}
			want := current
			if tc.wantPublished {
				want = next
			}
			assertLoadedRestoreAnchor(t, path, want)
			if tc.wantPublished {
				if retryErr := WriteRestoreAnchor(context.Background(), path, &current, next, RestoreAnchorWriteAdvance); !errors.Is(retryErr, ErrRestoreAnchorConflict) {
					t.Fatalf("retry with stale expected error = %v, want ErrRestoreAnchorConflict", retryErr)
				}
				assertLoadedRestoreAnchor(t, path, next)
				if retryErr := WriteRestoreAnchor(context.Background(), path, &next, next, RestoreAnchorWriteAdvance); retryErr != nil {
					t.Fatalf("retry after authoritative reread: %v", retryErr)
				}
			}
			temps, globErr := filepath.Glob(filepath.Join(filepath.Dir(path), ".restore-anchor-*.tmp"))
			if globErr != nil {
				t.Fatalf("Glob: %v", globErr)
			}
			if len(temps) != 0 {
				t.Fatalf("temporary files remain after failed write: %v", temps)
			}
		})
	}
}

func TestWriteRestoreAnchorCancellationBeforeRenameKeepsPriorEvidence(t *testing.T) {
	path := RestoreAnchorPath(t.TempDir())
	current := RestoreAnchor{
		Version:                 RestoreAnchorVersion1,
		Store:                   CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: 1},
		HighestAcceptedRevision: 3,
		HighestAcceptedSequence: 2,
	}
	if err := WriteRestoreAnchor(context.Background(), path, nil, current, RestoreAnchorWriteInitialize); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	next := current
	next.HighestAcceptedRevision++
	ctx, cancel := context.WithCancel(context.Background())
	ops := osRestoreAnchorFileOps
	ops.syncFile = func(file *os.File) error {
		if err := file.Sync(); err != nil {
			return err
		}
		cancel()
		return nil
	}
	err := writeRestoreAnchor(ctx, path, &current, next, RestoreAnchorWriteAdvance, ops)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("writeRestoreAnchor error = %v, want context.Canceled", err)
	}
	assertLoadedRestoreAnchor(t, path, current)
}

func TestWriteRestoreAnchorEqualRecordResyncsDurability(t *testing.T) {
	path := RestoreAnchorPath(t.TempDir())
	anchor := RestoreAnchor{
		Version:                 RestoreAnchorVersion1,
		Store:                   CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: 1},
		HighestAcceptedRevision: 3,
		HighestAcceptedSequence: 2,
	}
	if err := WriteRestoreAnchor(context.Background(), path, nil, anchor, RestoreAnchorWriteInitialize); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	ops := osRestoreAnchorFileOps
	var fileSyncs, directorySyncs int
	ops.syncFile = func(file *os.File) error {
		fileSyncs++
		return file.Sync()
	}
	ops.syncDirectory = func(path string) error {
		directorySyncs++
		return osRestoreAnchorFileOps.syncDirectory(path)
	}
	if err := writeRestoreAnchor(context.Background(), path, &anchor, anchor, RestoreAnchorWriteAdvance, ops); err != nil {
		t.Fatalf("writeRestoreAnchor equal record: %v", err)
	}
	if fileSyncs != 1 || directorySyncs != 1 {
		t.Fatalf("equal record syncs = file:%d directory:%d, want 1 each", fileSyncs, directorySyncs)
	}
}

func TestRestoreAnchorFileRejectsUnsafePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission and symlink contract")
	}
	anchor := RestoreAnchor{
		Version: RestoreAnchorVersion1,
		Store:   CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: 1},
	}
	wire, err := EncodeRestoreAnchor(anchor)
	if err != nil {
		t.Fatalf("EncodeRestoreAnchor: %v", err)
	}
	t.Run("broad parent mode", func(t *testing.T) {
		path := RestoreAnchorPath(t.TempDir())
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		if err := WriteRestoreAnchor(context.Background(), path, nil, anchor, RestoreAnchorWriteInitialize); !errors.Is(err, ErrRestoreAnchorUnsafePath) {
			t.Fatalf("WriteRestoreAnchor error = %v, want ErrRestoreAnchorUnsafePath", err)
		}
	})
	t.Run("broad file mode", func(t *testing.T) {
		path := RestoreAnchorPath(t.TempDir())
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(path, wire, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, _, err := LoadRestoreAnchor(context.Background(), path); !errors.Is(err, ErrRestoreAnchorUnsafePath) {
			t.Fatalf("LoadRestoreAnchor error = %v, want ErrRestoreAnchorUnsafePath", err)
		}
	})
	t.Run("symlink file", func(t *testing.T) {
		root := t.TempDir()
		path := RestoreAnchorPath(root)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		target := filepath.Join(root, "target")
		if err := os.WriteFile(target, wire, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		if _, _, err := LoadRestoreAnchor(context.Background(), path); !errors.Is(err, ErrRestoreAnchorUnsafePath) {
			t.Fatalf("LoadRestoreAnchor error = %v, want ErrRestoreAnchorUnsafePath", err)
		}
	})
}

func TestRestoreAnchorFileHonorsPreCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := RestoreAnchorPath(t.TempDir())
	anchor := RestoreAnchor{Version: RestoreAnchorVersion1, Store: CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: 1}}
	if _, _, err := LoadRestoreAnchor(ctx, path); !errors.Is(err, context.Canceled) {
		t.Errorf("LoadRestoreAnchor error = %v, want context.Canceled", err)
	}
	if err := WriteRestoreAnchor(ctx, path, nil, anchor, RestoreAnchorWriteInitialize); !errors.Is(err, context.Canceled) {
		t.Errorf("WriteRestoreAnchor error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled write created anchor: Stat error = %v", err)
	}
}

func TestLoadRestoreAnchorBoundsPhysicalRead(t *testing.T) {
	path := RestoreAnchorPath(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, make([]byte, MaxRestoreAnchorBytes+1), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, _, err := LoadRestoreAnchor(context.Background(), path); err == nil {
		t.Fatal("LoadRestoreAnchor accepted oversized file")
	}
}

func TestRestoreAnchorFileExplicitLifecycle(t *testing.T) {
	cityPath := t.TempDir()
	path := RestoreAnchorPath(cityPath)
	if path == cityPath || filepath.Dir(path) == cityPath {
		t.Fatalf("RestoreAnchorPath(%q) = %q, want dedicated runtime directory", cityPath, path)
	}

	if _, exists, err := LoadRestoreAnchor(context.Background(), path); err != nil || exists {
		t.Fatalf("initial LoadRestoreAnchor = (_, %t, %v), want missing", exists, err)
	}
	initial := RestoreAnchor{
		Version:                 RestoreAnchorVersion1,
		Store:                   CommandStoreBinding{StoreUUID: "provisioned-store", RestoreEpoch: 1},
		HighestAcceptedRevision: 7,
		HighestAcceptedSequence: 6,
	}
	if err := WriteRestoreAnchor(context.Background(), path, nil, initial, RestoreAnchorWriteInitialize); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	assertLoadedRestoreAnchor(t, path, initial)
	assertRestoreAnchorModes(t, path)

	advanced := initial
	advanced.HighestAcceptedRevision = 9
	advanced.HighestAcceptedSequence = 7
	if err := WriteRestoreAnchor(context.Background(), path, &initial, advanced, RestoreAnchorWriteAdvance); err != nil {
		t.Fatalf("advance: %v", err)
	}
	assertLoadedRestoreAnchor(t, path, advanced)

	recovered := advanced
	recovered.Store.RestoreEpoch = 3
	recovered.HighestAcceptedRevision = 2
	recovered.HighestAcceptedSequence = 1
	if err := WriteRestoreAnchor(context.Background(), path, &advanced, recovered, RestoreAnchorWriteRecovery); err != nil {
		t.Fatalf("recovery re-anchor: %v", err)
	}
	assertLoadedRestoreAnchor(t, path, recovered)
}

func TestWriteRestoreAnchorRejectsResetAndStaleExpectations(t *testing.T) {
	path := RestoreAnchorPath(t.TempDir())
	current := RestoreAnchor{
		Version:                 RestoreAnchorVersion1,
		Store:                   CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: 4},
		HighestAcceptedRevision: 11,
		HighestAcceptedSequence: 7,
	}
	if err := WriteRestoreAnchor(context.Background(), path, nil, current, RestoreAnchorWriteInitialize); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	tests := []struct {
		name     string
		expected *RestoreAnchor
		next     RestoreAnchor
		mode     RestoreAnchorWriteMode
	}{
		{name: "initialize over existing", expected: nil, next: current, mode: RestoreAnchorWriteInitialize},
		{name: "missing expected", expected: nil, next: current, mode: RestoreAnchorWriteAdvance},
		{name: "stale expected", expected: restoreAnchorTestPtr(withRestoreAnchorRevision(current, 10)), next: withRestoreAnchorRevision(current, 12), mode: RestoreAnchorWriteAdvance},
		{name: "normal revision rewind", expected: &current, next: withRestoreAnchorRevision(current, 10), mode: RestoreAnchorWriteAdvance},
		{name: "normal epoch change", expected: &current, next: withRestoreAnchorEpoch(current, 5), mode: RestoreAnchorWriteAdvance},
		{name: "recovery same epoch", expected: &current, next: withRestoreAnchorRevision(current, 12), mode: RestoreAnchorWriteRecovery},
		{name: "recovery epoch rewind", expected: &current, next: withRestoreAnchorEpoch(current, 3), mode: RestoreAnchorWriteRecovery},
		{name: "foreign normal", expected: &current, next: withRestoreAnchorStore(current, "store-b"), mode: RestoreAnchorWriteAdvance},
		{name: "foreign recovery", expected: &current, next: withRestoreAnchorStore(withRestoreAnchorEpoch(current, 5), "store-b"), mode: RestoreAnchorWriteRecovery},
		{name: "unknown mode", expected: &current, next: current, mode: RestoreAnchorWriteMode("future")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := WriteRestoreAnchor(context.Background(), path, tc.expected, tc.next, tc.mode); !errors.Is(err, ErrRestoreAnchorConflict) {
				t.Fatalf("WriteRestoreAnchor error = %v, want ErrRestoreAnchorConflict", err)
			}
			assertLoadedRestoreAnchor(t, path, current)
		})
	}
}

func TestWriteRestoreAnchorNeverOverwritesCorruptEvidence(t *testing.T) {
	path := RestoreAnchorPath(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	corrupt := []byte("not an anchor\n")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	next := RestoreAnchor{
		Version: RestoreAnchorVersion1,
		Store:   CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: 1},
	}
	if err := WriteRestoreAnchor(context.Background(), path, nil, next, RestoreAnchorWriteInitialize); err == nil || errors.Is(err, ErrRestoreAnchorConflict) {
		t.Fatalf("WriteRestoreAnchor corrupt existing error = %v, want decode failure", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(corrupt) {
		t.Fatalf("corrupt evidence was overwritten: got %q, want %q", got, corrupt)
	}
}

func assertLoadedRestoreAnchor(t *testing.T, path string, want RestoreAnchor) {
	t.Helper()
	got, exists, err := LoadRestoreAnchor(context.Background(), path)
	if err != nil {
		t.Fatalf("LoadRestoreAnchor(%q): %v", path, err)
	}
	if !exists || got != want {
		t.Fatalf("LoadRestoreAnchor(%q) = (%#v, %t), want (%#v, true)", path, got, exists, want)
	}
}

func assertRestoreAnchorModes(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	fileInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat anchor: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("anchor mode = %04o, want 0600", got)
	}
	dirInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Lstat anchor parent: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("anchor parent mode = %04o, want 0700", got)
	}
}

func withRestoreAnchorRevision(anchor RestoreAnchor, revision uint64) RestoreAnchor {
	anchor.HighestAcceptedRevision = revision
	return anchor
}

func withRestoreAnchorEpoch(anchor RestoreAnchor, epoch uint64) RestoreAnchor {
	anchor.Store.RestoreEpoch = epoch
	return anchor
}

func withRestoreAnchorStore(anchor RestoreAnchor, storeUUID string) RestoreAnchor {
	anchor.Store.StoreUUID = storeUUID
	return anchor
}

func restoreAnchorTestPtr(anchor RestoreAnchor) *RestoreAnchor {
	return &anchor
}
