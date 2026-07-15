//go:build cgo

package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	doltembed "github.com/dolthub/driver/v2"
)

var nativeDoltEmbeddedDatabaseName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func openNativeDoltEmbeddedSnapshotDB(ctx context.Context, dataDir, database, branch string) (*sql.DB, func() error, error) {
	if !nativeDoltEmbeddedDatabaseName.MatchString(database) {
		return nil, nil, fmt.Errorf("NativeDolt embedded snapshot database name %q is invalid: %w", database, ErrAtomicReadSnapshotUnsupported)
	}
	values := url.Values{}
	values.Set(doltembed.CommitNameParam, "gascity")
	values.Set(doltembed.CommitEmailParam, "gascity@local")
	values.Set(doltembed.MultiStatementsParam, "true")
	values.Set(doltembed.DatabaseParam, database)
	if os.PathSeparator == '\\' {
		dataDir = strings.ReplaceAll(dataDir, `\`, "/")
	}
	config, err := doltembed.ParseDSN("file://" + dataDir + "?" + values.Encode())
	if err != nil {
		return nil, nil, fmt.Errorf("parsing NativeDolt embedded snapshot DSN: %w", errors.Join(ErrAtomicReadSnapshotUnsupported, err))
	}
	connector, err := doltembed.NewConnector(config)
	if err != nil {
		return nil, nil, fmt.Errorf("opening NativeDolt embedded snapshot connector: %w", errors.Join(ErrAtomicReadSnapshotUnsupported, err))
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	cleanup := func() error {
		dbErr := db.Close()
		connectorErr := connector.Close()
		if errors.Is(dbErr, context.Canceled) {
			dbErr = nil
		}
		if errors.Is(connectorErr, context.Canceled) {
			connectorErr = nil
		}
		return errors.Join(dbErr, connectorErr)
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, nil, errors.Join(
			fmt.Errorf("pinging NativeDolt embedded snapshot database: %w", ErrAtomicReadSnapshotUnsupported),
			err,
			cleanup(),
		)
	}
	if _, err := db.ExecContext(ctx, "USE `"+database+"`"); err != nil {
		return nil, nil, errors.Join(
			fmt.Errorf("selecting NativeDolt embedded snapshot database: %w", ErrAtomicReadSnapshotUnsupported),
			err,
			cleanup(),
		)
	}
	if strings.TrimSpace(branch) != "" {
		branch = strings.ReplaceAll(strings.TrimSpace(branch), "'", "''")
		//nolint:gosec // database is identifier-validated and branch is SQL-literal escaped.
		if _, err := db.ExecContext(ctx, fmt.Sprintf("SET @@%s_head_ref = '%s'", database, branch)); err != nil {
			return nil, nil, errors.Join(
				fmt.Errorf("selecting NativeDolt embedded snapshot branch: %w", ErrAtomicReadSnapshotUnsupported),
				err,
				cleanup(),
			)
		}
	}
	return db, cleanup, nil
}
