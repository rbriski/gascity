package graphstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
)

// TestDialectSQLiteByteIdentity proves the dialect seam does not change any
// SQLite behavior: the default sqliteDialect returns the exact frozen DDL ladder,
// the historical sqlite_master probe, the historical busy mapping, and a no-op
// lockStream. If any of these drift, the "SQLite path is byte-identical" contract
// is broken.
func TestDialectSQLiteByteIdentity(t *testing.T) {
	d := sqliteDialect{}

	if d.name() != "sqlite" {
		t.Fatalf("name() = %q, want sqlite", d.name())
	}

	// The ladder is the same four rungs, same contents, as the package migrations.
	got := d.migrations()
	if len(got) != len(migrations) {
		t.Fatalf("migrations len = %d, want %d", len(got), len(migrations))
	}
	for i := range migrations {
		if got[i] != migrations[i] {
			t.Errorf("migrations[%d] diverged from the frozen ladder", i)
		}
	}

	// The probe is exactly the historical sqlite_master existence query.
	const wantProbe = `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='graph_meta'`
	if d.schemaProbe() != wantProbe {
		t.Errorf("schemaProbe()\n  got  %q\n  want %q", d.schemaProbe(), wantProbe)
	}

	// mapError is the historical SQLITE_BUSY text mapping, unchanged.
	if d.mapError(nil) != nil {
		t.Errorf("mapError(nil) != nil")
	}
	locked := errors.New("database is locked")
	if !errors.Is(d.mapError(locked), ErrBusy) {
		t.Errorf("mapError(database is locked) not ErrBusy")
	}
	other := errors.New("some other error")
	if errors.Is(d.mapError(other), ErrBusy) {
		t.Errorf("mapError(other) must not be ErrBusy")
	}

	// lockStream is a no-op (nil tx is fine because it must never touch it).
	if err := d.lockStream(context.Background(), nil, "gcj-root"); err != nil {
		t.Errorf("sqlite lockStream = %v, want nil no-op", err)
	}
}

// TestDialectPostgresMapError pins the Postgres SQLSTATE → sentinel mapping
// (blueprint §3.4) without needing a live server: synthetic pq.Errors classify to
// ErrBusy (lock timeout / deadlock / serialization) and the loud unique-violation
// backstops (ErrWrongExpectedVersion, ErrIdemTokenReuse); everything else passes
// through unchanged.
func TestDialectPostgresMapError(t *testing.T) {
	d := postgresDialect{}
	if d.name() != "postgres" {
		t.Fatalf("name() = %q, want postgres", d.name())
	}
	if d.mapError(nil) != nil {
		t.Errorf("mapError(nil) != nil")
	}

	cases := []struct {
		name string
		err  error
		want error // sentinel it must errors.Is, or nil for "unchanged"
	}{
		{"lock_timeout", &pq.Error{Code: "55P03"}, ErrBusy},
		{"deadlock", &pq.Error{Code: "40P01"}, ErrBusy},
		{"serialization", &pq.Error{Code: "40001"}, ErrBusy},
		{"pk_backstop", &pq.Error{Code: "23505", Constraint: "journal_pkey"}, ErrWrongExpectedVersion},
		{"idem_backstop", &pq.Error{Code: "23505", Constraint: "journal_idem"}, ErrIdemTokenReuse},
		{"other_unique", &pq.Error{Code: "23505", Constraint: "node_metadata_pkey"}, nil},
		{"unrelated", &pq.Error{Code: "42P01"}, nil}, // undefined_table, unchanged
		{"non_pq", errors.New("plain"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := d.mapError(tc.err)
			if tc.want == nil {
				// unchanged: must not be any busy/typed sentinel and must wrap the input
				if errors.Is(got, ErrBusy) || errors.Is(got, ErrWrongExpectedVersion) || errors.Is(got, ErrIdemTokenReuse) {
					t.Errorf("mapError(%v) mapped to a sentinel, want unchanged", tc.err)
				}
				//nolint:errorlint // identity check: the unchanged path must return the exact same error value, not a wrap
				if got != tc.err {
					t.Errorf("mapError(%v) = %v, want the input unchanged", tc.err, got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Errorf("mapError(%v) = %v, want errors.Is %v", tc.err, got, tc.want)
			}
			// The original driver error is preserved in the chain.
			if !errors.Is(got, tc.err) {
				t.Errorf("mapError(%v) dropped the original error from the chain", tc.err)
			}
		})
	}
}

// TestRedactDSN proves passwords are stripped from every DSN form and that a
// credential-free string (notably a SQLite file path) is returned byte-for-byte.
func TestRedactDSN(t *testing.T) {
	const secret = "SUPERSECRET"
	cases := []struct {
		name       string
		in         string
		mustHide   bool
		wantExact  string // when non-empty, redaction must equal this exactly
		mustRemain string // when non-empty, this substring must survive (e.g. user, host)
	}{
		{
			name:       "url_userinfo",
			in:         "postgres://alice:" + secret + "@db.example:5432/city?sslmode=disable",
			mustHide:   true,
			mustRemain: "alice",
		},
		{
			name:      "url_no_password",
			in:        "postgres://alice@db.example:5432/city?sslmode=disable",
			wantExact: "postgres://alice@db.example:5432/city?sslmode=disable",
		},
		{
			name:     "keyword_bare",
			in:       "host=db.example port=5432 user=alice password=" + secret + " dbname=city",
			mustHide: true,
		},
		{
			name:     "keyword_quoted",
			in:       "host=db.example user=alice password='" + secret + "' dbname=city",
			mustHide: true,
		},
		{
			name:      "sqlite_path_unchanged",
			in:        "/var/city/.gc/graph/journal.db",
			wantExact: "/var/city/.gc/graph/journal.db",
		},
		{
			name:      "sqlite_dsn_unchanged",
			in:        "file:/var/city/journal.db?_pragma=busy_timeout(5000)&_txlock=immediate",
			wantExact: "file:/var/city/journal.db?_pragma=busy_timeout(5000)&_txlock=immediate",
		},
		{
			name:     "embedded_in_error_message",
			in:       `pq: could not connect to postgres://alice:` + secret + `@db.example:5432/city`,
			mustHide: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactDSN(tc.in)
			if tc.mustHide && strings.Contains(got, secret) {
				t.Fatalf("redactDSN leaked the password: %q", got)
			}
			if tc.wantExact != "" && got != tc.wantExact {
				t.Errorf("redactDSN(%q)\n  got  %q\n  want %q", tc.in, got, tc.wantExact)
			}
			if tc.mustRemain != "" && !strings.Contains(got, tc.mustRemain) {
				t.Errorf("redactDSN(%q) dropped %q: %q", tc.in, tc.mustRemain, got)
			}
		})
	}
}

// TestOpenPostgresDSNRedaction proves a password in the DSN never reaches an
// error string when openPostgres fails to connect. It needs no live server: the
// DSN points at a closed port so the connect fails fast, and the returned error
// must not contain the password.
func TestOpenPostgresDSNRedaction(t *testing.T) {
	const secret = "hunter2topsecret"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dsns := []string{
		fmt.Sprintf("postgres://u:%s@127.0.0.1:1/db?sslmode=disable&connect_timeout=2", secret),
		fmt.Sprintf("host=127.0.0.1 port=1 user=u password=%s dbname=db sslmode=disable connect_timeout=2", secret),
	}
	for _, dsn := range dsns {
		_, err := openPostgres(ctx, dsn, Options{}, nil)
		if err == nil {
			t.Fatalf("openPostgres against a closed port unexpectedly succeeded")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("connect error leaked the password: %v", err)
		}
	}
}
