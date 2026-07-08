// Package pgqmark wraps the lib/pq PostgreSQL driver with a thin
// database/sql/driver shim that rewrites `?` positional placeholders into
// PostgreSQL's `$1, $2, …` form before delegating.
//
// It exists so the graphstore journal engine and the beads JournalStore façade
// can keep every one of their ~80 SQL strings byte-identical across the SQLite
// and Postgres backends: SQLite (modernc.org/sqlite) speaks `?`, lib/pq speaks
// `$N`, and this shim closes the gap at the driver boundary instead of forking
// the queries. The rewrite is literal-aware — a `?` inside a single-quoted
// string, an E-string, a double-quoted identifier, a dollar-quoted body
// ($tag$…$tag$), or a line/block comment is left untouched.
//
// It is intended for `?`-only SQL: every one of the substrate's query strings
// uses positional `?` exclusively and contains no `?`-bearing operators, so the
// literal-blind renumbering is exact. Mixing PG-native `$N` placeholders with
// `?` in a single statement is UNSUPPORTED — the `?` are renumbered from `$1`
// independently of any literal `$N`, so a construct like `WHERE a=$1 AND b=?`
// collides at `$1` and fails loudly at bind (never silently). A `$N` token that
// lives only inside a skipped region — e.g. the `$fn$…$fn$` bodies of the DDL —
// is not a placeholder and passes through unchanged.
//
// The shim advertises ExecerContext/QueryerContext so no-argument statements
// (the multi-statement DDL rungs) reach lib/pq's simple-query protocol, which
// is the only path that accepts multiple commands in one round trip; the
// extended (Prepare) protocol rejects them. Parameterized statements flow
// through the extended protocol with `$N` binds exactly as lib/pq expects.
package pgqmark

import (
	"context"
	"database/sql"
	"database/sql/driver"

	"github.com/lib/pq"
)

// DriverName is the name registered with database/sql. sql.Open(DriverName, dsn)
// yields a connection that rewrites `?` placeholders and otherwise behaves as
// lib/pq.
const DriverName = "graphstore-pgqmark"

func init() { sql.Register(DriverName, Driver{}) }

// Driver is the registered database/sql driver. It wraps lib/pq.
type Driver struct{}

var (
	_ driver.Driver        = Driver{}
	_ driver.DriverContext = Driver{}
)

// Open implements driver.Driver by establishing a single lib/pq connection and
// wrapping it in the qmark-rewriting shim. sql.Open uses this path.
func (Driver) Open(name string) (driver.Conn, error) {
	base, err := pq.NewConnector(name)
	if err != nil {
		return nil, err
	}
	raw, err := base.Connect(context.Background())
	if err != nil {
		return nil, err
	}
	return &conn{raw: raw}, nil
}

// OpenConnector implements driver.DriverContext so sql.Open reuses one parsed
// DSN across pooled connections.
func (Driver) OpenConnector(name string) (driver.Connector, error) {
	base, err := pq.NewConnector(name)
	if err != nil {
		return nil, err
	}
	return &Connector{base: base}, nil
}

// Connector is a database/sql driver.Connector that wraps a lib/pq connector and
// applies the qmark rewrite to every connection it hands out. An optional
// onConnect hook runs against each freshly established connection (used by the
// graphstore Postgres opener to set lock_timeout and other per-session GUCs);
// lib/pq's ResetSession does not clear session state, so a SET applied here
// persists for the pooled connection's lifetime.
type Connector struct {
	base      driver.Connector
	onConnect func(context.Context, driver.Conn) error
}

var _ driver.Connector = (*Connector)(nil)

// NewConnector builds a Connector for dsn (URL or keyword form; lib/pq parses
// both). onConnect, when non-nil, runs against each new connection before it is
// returned to the pool; an error from it fails the connection.
func NewConnector(dsn string, onConnect func(context.Context, driver.Conn) error) (*Connector, error) {
	base, err := pq.NewConnector(dsn)
	if err != nil {
		return nil, err
	}
	return &Connector{base: base, onConnect: onConnect}, nil
}

// Connect establishes a new connection, wraps it, and runs the onConnect hook.
func (c *Connector) Connect(ctx context.Context) (driver.Conn, error) {
	raw, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	w := &conn{raw: raw}
	if c.onConnect != nil {
		if err := c.onConnect(ctx, w); err != nil {
			_ = raw.Close()
			return nil, err
		}
	}
	return w, nil
}

// Driver returns the qmark driver.
func (c *Connector) Driver() driver.Driver { return Driver{} }

// conn wraps a lib/pq driver.Conn, rewriting `?` → `$N` on every query-bearing
// call before delegating. It forwards the optional driver interfaces lib/pq
// implements (context exec/query/prepare, BeginTx isolation, Ping, session
// reset, validity) so database/sql keeps its fast paths.
type conn struct {
	raw driver.Conn
}

var (
	_ driver.Conn               = (*conn)(nil)
	_ driver.ConnPrepareContext = (*conn)(nil)
	_ driver.ConnBeginTx        = (*conn)(nil)
	_ driver.ExecerContext      = (*conn)(nil)
	_ driver.QueryerContext     = (*conn)(nil)
	_ driver.Pinger             = (*conn)(nil)
	_ driver.SessionResetter    = (*conn)(nil)
	_ driver.Validator          = (*conn)(nil)
)

// Prepare rewrites and delegates.
func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return c.raw.Prepare(rewrite(query))
}

// PrepareContext rewrites and delegates to lib/pq's context-aware prepare.
func (c *conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if p, ok := c.raw.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, rewrite(query))
	}
	return c.raw.Prepare(rewrite(query))
}

// Close delegates.
func (c *conn) Close() error { return c.raw.Close() }

// Begin delegates. Retained for the driver.Conn contract; database/sql prefers
// BeginTx.
func (c *conn) Begin() (driver.Tx, error) {
	return c.raw.Begin() //nolint:staticcheck // required by driver.Conn
}

// BeginTx delegates, passing isolation/read-only options through to lib/pq so
// the caller's transaction options (or the absence of them) are honored.
func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.raw.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return c.raw.Begin() //nolint:staticcheck // fallback for a driver without BeginTx
}

// ExecContext rewrites and delegates. No-argument statements reach lib/pq's
// simple-query protocol, which accepts multi-statement DDL.
func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	ec, ok := c.raw.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return ec.ExecContext(ctx, rewrite(query), args)
}

// QueryContext rewrites and delegates.
func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	qc, ok := c.raw.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return qc.QueryContext(ctx, rewrite(query), args)
}

// Ping delegates when lib/pq supports it.
func (c *conn) Ping(ctx context.Context) error {
	if p, ok := c.raw.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

// ResetSession delegates when lib/pq supports it.
func (c *conn) ResetSession(ctx context.Context) error {
	if r, ok := c.raw.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

// IsValid delegates when lib/pq supports it.
func (c *conn) IsValid() bool {
	if v, ok := c.raw.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}
