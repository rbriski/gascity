//go:build !cgo

package beads

import (
	"context"
	"database/sql"
	"fmt"
)

func openNativeDoltEmbeddedSnapshotDB(context.Context, string, string, string) (*sql.DB, func() error, error) {
	return nil, nil, fmt.Errorf("NativeDolt embedded snapshot database requires CGO: %w", ErrAtomicReadSnapshotUnsupported)
}
