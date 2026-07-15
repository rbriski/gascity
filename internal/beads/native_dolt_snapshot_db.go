package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
)

type nativeDoltEmbeddedSnapshotLocator interface {
	Path() string
	CLIDir() string
	CurrentBranch(context.Context) (string, error)
}

func openNativeDoltSnapshotDB(ctx context.Context, storage any) (*sql.DB, func() error, error) {
	if accessor, ok := storage.(rawDBGetter); ok && accessor.DB() != nil {
		return accessor.DB(), func() error { return nil }, nil
	}
	locator, ok := storage.(nativeDoltEmbeddedSnapshotLocator)
	if !ok {
		return nil, nil, fmt.Errorf("NativeDolt storage %T has no raw or embedded snapshot database: %w", storage, ErrAtomicReadSnapshotUnsupported)
	}
	dataDir := filepath.Clean(locator.Path())
	cliDir := filepath.Clean(locator.CLIDir())
	if dataDir == "." || cliDir == "." || filepath.Dir(cliDir) != dataDir {
		return nil, nil, fmt.Errorf("NativeDolt embedded snapshot locator is non-canonical (path %q, CLI dir %q): %w", locator.Path(), locator.CLIDir(), ErrAtomicReadSnapshotUnsupported)
	}
	branch, err := locator.CurrentBranch(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving NativeDolt embedded snapshot branch: %w", errors.Join(ErrAtomicReadSnapshotUnsupported, err))
	}
	return openNativeDoltEmbeddedSnapshotDB(ctx, dataDir, filepath.Base(cliDir), branch)
}
