package nudgequeue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/filelock"
)

const (
	restoreAnchorDirectoryName = "nudge-command-authority"
	restoreAnchorFileName      = "restore-anchor.json"
)

var (
	// ErrRestoreAnchorConflict reports a stale expected anchor, an attempted
	// monotonicity violation, or use of the wrong explicit write mode.
	ErrRestoreAnchorConflict = errors.New("nudge command restore anchor write conflict")
	// ErrRestoreAnchorBusy reports that another process currently owns the
	// short anchor write critical section. Callers may reread and retry.
	ErrRestoreAnchorBusy = errors.New("nudge command restore anchor is busy")
	// ErrRestoreAnchorUnsafePath reports a symlink, non-regular anchor, or
	// permissions that would expose independent recovery evidence.
	ErrRestoreAnchorUnsafePath = errors.New("nudge command restore anchor path is unsafe")
	// ErrRestoreAnchorDurabilityUncertain reports that atomic publication
	// succeeded but syncing the containing directory failed. Callers must freeze
	// effects and reread; they must not assume either the old or new crash state.
	ErrRestoreAnchorDurabilityUncertain = errors.New("nudge command restore anchor durability is uncertain")
)

// RestoreAnchorWriteMode makes first provisioning and restore recovery
// syntactically distinct from ordinary monotonic high-water advancement.
type RestoreAnchorWriteMode string

const (
	// RestoreAnchorWriteInitialize installs an anchor only when none exists and
	// expected is nil. Repository verification must never select this mode.
	RestoreAnchorWriteInitialize RestoreAnchorWriteMode = "initialize"
	// RestoreAnchorWriteAdvance advances a revision within the same store UUID
	// and restore epoch.
	RestoreAnchorWriteAdvance RestoreAnchorWriteMode = "advance"
	// RestoreAnchorWriteRecovery advances the restore epoch for an explicit
	// recovery operation. The recovery owner must quarantine recovered effects
	// and satisfy RestoreAnchorDecision.MinimumRecoveryEpoch before calling it.
	// This write alone never authorizes effects and ordinary verification must
	// never select it.
	RestoreAnchorWriteRecovery RestoreAnchorWriteMode = "recovery"
)

type restoreAnchorFileOps struct {
	syncFile      func(*os.File) error
	rename        func(string, string) error
	syncDirectory func(string) error
}

type restoreAnchorLockOps struct {
	lstat    func(string) (os.FileInfo, error)
	openFile func(string, int, os.FileMode) (*os.File, error)
}

var osRestoreAnchorFileOps = restoreAnchorFileOps{
	syncFile: func(file *os.File) error {
		return file.Sync()
	},
	rename: os.Rename,
	syncDirectory: func(path string) error {
		directory, err := os.Open(path)
		if err != nil {
			return err
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		return errors.Join(syncErr, closeErr)
	},
}

var osRestoreAnchorLockOps = restoreAnchorLockOps{
	lstat:    os.Lstat,
	openFile: os.OpenFile,
}

// RestoreAnchorPath returns the canonical independent local anchor path for a
// city. The path selects storage only; no path component becomes store
// identity or restore authority.
func RestoreAnchorPath(cityPath string) string {
	return citylayout.RuntimePath(cityPath, restoreAnchorDirectoryName, restoreAnchorFileName)
}

// LoadRestoreAnchor loads and strictly validates one independent anchor. A
// missing file is reported as exists=false; corruption, unsafe permissions,
// cancellation, and I/O failures are errors and are never treated as a reset.
func LoadRestoreAnchor(ctx context.Context, path string) (anchor RestoreAnchor, exists bool, err error) {
	if ctx == nil {
		return RestoreAnchor{}, false, errors.New("loading nudge command restore anchor: nil context")
	}
	if err := ctx.Err(); err != nil {
		return RestoreAnchor{}, false, fmt.Errorf("loading nudge command restore anchor: %w", err)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return RestoreAnchor{}, false, nil
	}
	if err != nil {
		return RestoreAnchor{}, false, fmt.Errorf("loading nudge command restore anchor: lstat: %w", err)
	}
	if err := validateRestoreAnchorDirectory(filepath.Dir(path)); err != nil {
		return RestoreAnchor{}, false, fmt.Errorf("loading nudge command restore anchor: %w", err)
	}
	if err := validateRestoreAnchorFileInfo(info); err != nil {
		return RestoreAnchor{}, false, fmt.Errorf("loading nudge command restore anchor: %w", err)
	}

	file, err := os.Open(path)
	if err != nil {
		return RestoreAnchor{}, false, fmt.Errorf("loading nudge command restore anchor: open: %w", err)
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return RestoreAnchor{}, false, fmt.Errorf("loading nudge command restore anchor: stat opened file: %w", statErr)
	}
	if !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return RestoreAnchor{}, false, fmt.Errorf("loading nudge command restore anchor: %w: file changed during open", ErrRestoreAnchorUnsafePath)
	}
	if err := validateRestoreAnchorFileInfo(openedInfo); err != nil {
		_ = file.Close()
		return RestoreAnchor{}, false, fmt.Errorf("loading nudge command restore anchor: opened file: %w", err)
	}
	wire, readErr := io.ReadAll(io.LimitReader(file, MaxRestoreAnchorBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return RestoreAnchor{}, false, fmt.Errorf("loading nudge command restore anchor: read: %w", errors.Join(readErr, closeErr))
	}
	if len(wire) > MaxRestoreAnchorBytes {
		return RestoreAnchor{}, false, fmt.Errorf("loading nudge command restore anchor: record exceeds %d bytes", MaxRestoreAnchorBytes)
	}
	if err := ctx.Err(); err != nil {
		return RestoreAnchor{}, false, fmt.Errorf("loading nudge command restore anchor: %w", err)
	}
	anchor, err = DecodeRestoreAnchor(wire)
	if err != nil {
		return RestoreAnchor{}, false, fmt.Errorf("loading nudge command restore anchor: %w", err)
	}
	return anchor, true, nil
}

// WriteRestoreAnchor atomically publishes next after verifying the exact
// expected on-disk record and the selected monotonic transition. It fsyncs the
// temporary file before rename and the parent directory afterward. A corrupt
// or unreadable existing anchor is never overwritten.
func WriteRestoreAnchor(ctx context.Context, path string, expected *RestoreAnchor, next RestoreAnchor, mode RestoreAnchorWriteMode) error {
	if mode == RestoreAnchorWriteRecovery {
		return fmt.Errorf("writing nudge command restore anchor: %w: recovery requires an observed epoch floor", ErrRestoreAnchorConflict)
	}
	return writeRestoreAnchor(ctx, path, expected, next, mode, 0, osRestoreAnchorFileOps)
}

// WriteRestoreAnchorRecovery publishes an explicit recovered lineage only when
// next is at or above the minimum epoch retained by the recovery decision.
// The caller must quarantine old-epoch work before invoking this function.
func WriteRestoreAnchorRecovery(ctx context.Context, path string, expected *RestoreAnchor, next RestoreAnchor, minimumEpoch uint64) error {
	return writeRestoreAnchor(ctx, path, expected, next, RestoreAnchorWriteRecovery, minimumEpoch, osRestoreAnchorFileOps)
}

func writeRestoreAnchor(ctx context.Context, path string, expected *RestoreAnchor, next RestoreAnchor, mode RestoreAnchorWriteMode, recoveryMinimumEpoch uint64, ops restoreAnchorFileOps) error {
	if ctx == nil {
		return errors.New("writing nudge command restore anchor: nil context")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("writing nudge command restore anchor: %w", err)
	}
	if err := ValidateRestoreAnchor(next); err != nil {
		return fmt.Errorf("writing nudge command restore anchor: %w", err)
	}
	var expectedCopy *RestoreAnchor
	if expected != nil {
		expectedValue := *expected
		if err := ValidateRestoreAnchor(expectedValue); err != nil {
			return fmt.Errorf("writing nudge command restore anchor: %w: invalid expected anchor: %w", ErrRestoreAnchorConflict, err)
		}
		expectedCopy = &expectedValue
	}
	if err := validateRestoreAnchorFileOps(ops); err != nil {
		return fmt.Errorf("writing nudge command restore anchor: %w", err)
	}
	parent := filepath.Dir(path)
	if err := ensureRestoreAnchorDirectory(parent, ops.syncDirectory); err != nil {
		return fmt.Errorf("writing nudge command restore anchor: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("writing nudge command restore anchor: %w", err)
	}

	lock, err := acquireRestoreAnchorLock(path)
	if err != nil {
		return err
	}
	defer func() {
		_ = filelock.Unlock(lock)
		_ = lock.Close()
	}()

	current, exists, err := LoadRestoreAnchor(ctx, path)
	if err != nil {
		return err
	}
	if err := validateRestoreAnchorExpected(current, exists, expectedCopy); err != nil {
		return err
	}
	if err := validateRestoreAnchorTransition(current, exists, next, mode, recoveryMinimumEpoch); err != nil {
		return err
	}
	if exists && current == next {
		return syncExistingRestoreAnchor(ctx, path, parent, current, ops)
	}
	wire, err := EncodeRestoreAnchor(next)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(parent, ".restore-anchor-*.tmp")
	if err != nil {
		return fmt.Errorf("writing nudge command restore anchor: create temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	published := false
	defer func() {
		if !published {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("writing nudge command restore anchor: chmod temporary file: %w", err)
	}
	if written, err := temporary.Write(wire); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("writing nudge command restore anchor: write temporary file: %w", err)
	} else if written != len(wire) {
		_ = temporary.Close()
		return fmt.Errorf("writing nudge command restore anchor: write temporary file: %w", io.ErrShortWrite)
	}
	if err := ops.syncFile(temporary); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("writing nudge command restore anchor: sync temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("writing nudge command restore anchor: close temporary file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("writing nudge command restore anchor: %w", err)
	}
	if err := ops.rename(temporaryPath, path); err != nil {
		return fmt.Errorf("writing nudge command restore anchor: publish: %w", errors.Join(ErrRestoreAnchorDurabilityUncertain, err))
	}
	published = true
	if err := ops.syncDirectory(parent); err != nil {
		return fmt.Errorf("writing nudge command restore anchor: sync parent: %w", errors.Join(ErrRestoreAnchorDurabilityUncertain, err))
	}
	return nil
}

func syncExistingRestoreAnchor(ctx context.Context, path, parent string, expected RestoreAnchor, ops restoreAnchorFileOps) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("writing nudge command restore anchor: %w", err)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("writing nudge command restore anchor: open equal anchor: %w", err)
	}
	if err := ops.syncFile(file); err != nil {
		_ = file.Close()
		return fmt.Errorf("writing nudge command restore anchor: sync equal anchor: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("writing nudge command restore anchor: close equal anchor: %w", err)
	}
	if err := ops.syncDirectory(parent); err != nil {
		return fmt.Errorf("writing nudge command restore anchor: sync equal parent: %w", errors.Join(ErrRestoreAnchorDurabilityUncertain, err))
	}
	current, exists, err := LoadRestoreAnchor(ctx, path)
	if err != nil {
		return err
	}
	if !exists || current != expected {
		return fmt.Errorf("writing nudge command restore anchor: %w: equal anchor changed during durability confirmation", ErrRestoreAnchorConflict)
	}
	return nil
}

func validateRestoreAnchorExpected(current RestoreAnchor, exists bool, expected *RestoreAnchor) error {
	if expected == nil {
		if exists {
			return fmt.Errorf("writing nudge command restore anchor: %w: expected missing anchor", ErrRestoreAnchorConflict)
		}
		return nil
	}
	if !exists || current != *expected {
		return fmt.Errorf("writing nudge command restore anchor: %w: on-disk anchor differs from expected", ErrRestoreAnchorConflict)
	}
	return nil
}

func validateRestoreAnchorTransition(current RestoreAnchor, exists bool, next RestoreAnchor, mode RestoreAnchorWriteMode, recoveryMinimumEpoch uint64) error {
	switch mode {
	case RestoreAnchorWriteInitialize:
		if recoveryMinimumEpoch != 0 || exists {
			return fmt.Errorf("writing nudge command restore anchor: %w: initialization requires a missing anchor", ErrRestoreAnchorConflict)
		}
		return nil
	case RestoreAnchorWriteAdvance:
		if recoveryMinimumEpoch != 0 || !exists || next.Store != current.Store || next.HighestAcceptedRevision < current.HighestAcceptedRevision || next.HighestAcceptedSequence < current.HighestAcceptedSequence {
			return fmt.Errorf("writing nudge command restore anchor: %w: ordinary advance must retain store lineage and not lower revision or sequence", ErrRestoreAnchorConflict)
		}
		return nil
	case RestoreAnchorWriteRecovery:
		if recoveryMinimumEpoch == 0 || !exists || next.Store.StoreUUID != current.Store.StoreUUID || next.Store.RestoreEpoch <= current.Store.RestoreEpoch || next.Store.RestoreEpoch < recoveryMinimumEpoch {
			return fmt.Errorf("writing nudge command restore anchor: %w: recovery must retain store identity and advance to at least observed epoch floor %d", ErrRestoreAnchorConflict, recoveryMinimumEpoch)
		}
		return nil
	default:
		return fmt.Errorf("writing nudge command restore anchor: %w: unknown write mode %q", ErrRestoreAnchorConflict, mode)
	}
}

func ensureRestoreAnchorDirectory(path string, syncDirectory func(string) error) error {
	info, err := os.Lstat(path)
	if err == nil {
		return validateRestoreAnchorDirectoryInfo(info)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("lstat anchor directory: %w", err)
	}
	var missing []string
	for cursor := path; ; cursor = filepath.Dir(cursor) {
		info, err := os.Lstat(cursor)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				resolved, statErr := os.Stat(cursor)
				if statErr != nil || !resolved.IsDir() {
					return fmt.Errorf("%w: anchor directory ancestor is not a directory", ErrRestoreAnchorUnsafePath)
				}
			} else if !info.IsDir() {
				return fmt.Errorf("%w: anchor directory ancestor is not a directory", ErrRestoreAnchorUnsafePath)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("lstat anchor directory ancestor: %w", err)
		}
		missing = append(missing, cursor)
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return fmt.Errorf("create anchor directory: no existing filesystem root for %q", path)
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		directory := missing[i]
		if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create anchor directory %q: %w", directory, err)
		}
		createdInfo, err := os.Lstat(directory)
		if err != nil {
			return fmt.Errorf("lstat created anchor directory %q: %w", directory, err)
		}
		if err := validateRestoreAnchorDirectoryInfo(createdInfo); err != nil {
			return err
		}
		if err := syncDirectory(filepath.Dir(directory)); err != nil {
			return fmt.Errorf("sync parent of new anchor directory %q: %w", directory, err)
		}
	}
	info, err = os.Lstat(path)
	if err != nil {
		return fmt.Errorf("lstat final anchor directory: %w", err)
	}
	return validateRestoreAnchorDirectoryInfo(info)
}

func validateRestoreAnchorDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("lstat anchor directory: %w", err)
	}
	return validateRestoreAnchorDirectoryInfo(info)
}

func validateRestoreAnchorDirectoryInfo(info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: anchor parent is not a real directory", ErrRestoreAnchorUnsafePath)
	}
	if runtime.GOOS != "windows" && (info.Mode().Perm() != 0o700 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0) {
		return fmt.Errorf("%w: anchor parent mode is %v, want 0700", ErrRestoreAnchorUnsafePath, info.Mode())
	}
	return nil
}

func validateRestoreAnchorFileInfo(info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: anchor is not a regular file", ErrRestoreAnchorUnsafePath)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		return fmt.Errorf("%w: anchor mode is %v, want 0600", ErrRestoreAnchorUnsafePath, info.Mode())
	}
	if info.Size() > MaxRestoreAnchorBytes {
		return fmt.Errorf("anchor record exceeds %d bytes", MaxRestoreAnchorBytes)
	}
	return nil
}

func acquireRestoreAnchorLock(path string) (*os.File, error) {
	return acquireRestoreAnchorLockWithOps(path, osRestoreAnchorLockOps)
}

func acquireRestoreAnchorLockWithOps(path string, ops restoreAnchorLockOps) (*os.File, error) {
	if ops.lstat == nil || ops.openFile == nil {
		return nil, errors.New("writing nudge command restore anchor: lock filesystem seam is incomplete")
	}
	lockPath := path + ".lock"
	before, statErr := ops.lstat(lockPath)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("writing nudge command restore anchor: lstat lock: %w", statErr)
	}
	if statErr == nil {
		if err := validateRestoreAnchorFileInfo(before); err != nil {
			return nil, fmt.Errorf("writing nudge command restore anchor: unsafe lock: %w", err)
		}
	}
	openFlags := os.O_CREATE | os.O_RDWR
	if errors.Is(statErr, os.ErrNotExist) {
		openFlags |= os.O_EXCL
	}
	lock, err := ops.openFile(lockPath, openFlags, 0o600)
	if err != nil {
		if errors.Is(statErr, os.ErrNotExist) && errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("writing nudge command restore anchor: %w: lock appeared during creation", ErrRestoreAnchorUnsafePath)
		}
		return nil, fmt.Errorf("writing nudge command restore anchor: open lock: %w", err)
	}
	opened, err := lock.Stat()
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("writing nudge command restore anchor: stat lock: %w", err)
	}
	live, err := ops.lstat(lockPath)
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("writing nudge command restore anchor: %w: lstat live lock: %w", ErrRestoreAnchorUnsafePath, err)
	}
	if err := validateRestoreAnchorFileInfo(live); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("writing nudge command restore anchor: unsafe live lock: %w", err)
	}
	if !os.SameFile(live, opened) || statErr == nil && !os.SameFile(before, live) {
		_ = lock.Close()
		return nil, fmt.Errorf("writing nudge command restore anchor: %w: lock pathname and opened file differ", ErrRestoreAnchorUnsafePath)
	}
	acquired, err := filelock.TryLock(lock, filelock.Exclusive)
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("writing nudge command restore anchor: lock: %w", err)
	}
	if !acquired {
		_ = lock.Close()
		return nil, fmt.Errorf("writing nudge command restore anchor: %w", ErrRestoreAnchorBusy)
	}
	return lock, nil
}

func validateRestoreAnchorFileOps(ops restoreAnchorFileOps) error {
	if ops.syncFile == nil || ops.rename == nil || ops.syncDirectory == nil {
		return errors.New("restore anchor filesystem seam is incomplete")
	}
	return nil
}
