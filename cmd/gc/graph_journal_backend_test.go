package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beads"
)

// writeGraphBackendMarker writes <city>/.gc/graph/.beads/config.yaml with the
// given content, opting the city into the graph scope.
func writeGraphBackendMarker(t *testing.T, cityPath, content string) {
	t.Helper()
	dir := filepath.Join(cityPath, ".gc", "graph", ".beads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir graph scope: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
}

func TestParseGraphJournalBackendConfig(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		backend graphJournalBackend
		dsnEnv  string
		cred    string
		wantErr bool
	}{
		{name: "historical provider marker", yaml: "provider: journal\n", backend: graphJournalBackendSQLite},
		{name: "empty file", yaml: "", backend: graphJournalBackendSQLite},
		{name: "explicit sqlite", yaml: "backend: sqlite\n", backend: graphJournalBackendSQLite},
		{name: "sqlite whitespace", yaml: "backend: \"  sqlite  \"\n", backend: graphJournalBackendSQLite},
		{
			name:    "postgres via dsn_env",
			yaml:    "provider: journal\nbackend: postgres\npostgres:\n  dsn_env: GC_GRAPH_PG_DSN\n",
			backend: graphJournalBackendPostgres,
			dsnEnv:  "GC_GRAPH_PG_DSN",
		},
		{
			name:    "postgres via credential_command",
			yaml:    "backend: postgres\npostgres:\n  credential_command: eia-graph-cred --audience graph\n",
			backend: graphJournalBackendPostgres,
			cred:    "eia-graph-cred --audience graph",
		},
		{
			name:    "postgres with both sources",
			yaml:    "backend: postgres\npostgres:\n  dsn_env: GC_GRAPH_PG_DSN\n  credential_command: helper\n",
			backend: graphJournalBackendPostgres,
			dsnEnv:  "GC_GRAPH_PG_DSN",
			cred:    "helper",
		},
		{name: "unknown backend is loud", yaml: "backend: mysql\n", wantErr: true},
		{name: "malformed yaml is loud", yaml: "backend: [unterminated\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseGraphJournalBackendConfig([]byte(tc.yaml))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseGraphJournalBackendConfig(%q) = %+v, want error", tc.yaml, cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGraphJournalBackendConfig(%q) error = %v", tc.yaml, err)
			}
			if cfg.backend != tc.backend {
				t.Fatalf("backend = %q, want %q", cfg.backend, tc.backend)
			}
			if cfg.dsnEnv != tc.dsnEnv {
				t.Fatalf("dsnEnv = %q, want %q", cfg.dsnEnv, tc.dsnEnv)
			}
			if cfg.credentialCommand != tc.cred {
				t.Fatalf("credentialCommand = %q, want %q", cfg.credentialCommand, tc.cred)
			}
		})
	}
}

func TestResolvePostgresDSNFromEnv(t *testing.T) {
	const dsn = "postgres://u:pw@db.example:5432/city?sslmode=disable"
	t.Setenv("GC_GRAPH_TEST_DSN_RESOLVE", dsn)
	cfg := graphJournalBackendConfig{backend: graphJournalBackendPostgres, dsnEnv: "GC_GRAPH_TEST_DSN_RESOLVE"}
	got, err := cfg.resolvePostgresDSN(context.Background())
	if err != nil {
		t.Fatalf("resolvePostgresDSN: %v", err)
	}
	if got != dsn {
		t.Fatalf("dsn = %q, want %q", got, dsn)
	}
}

func TestResolvePostgresDSNEnvUnsetIsLoudAndDoesNotLeakName(t *testing.T) {
	t.Setenv("GC_GRAPH_TEST_DSN_MISSING", "") // empty is treated as unset
	cfg := graphJournalBackendConfig{backend: graphJournalBackendPostgres, dsnEnv: "GC_GRAPH_TEST_DSN_MISSING"}
	_, err := cfg.resolvePostgresDSN(context.Background())
	if err == nil {
		t.Fatal("resolvePostgresDSN with an unset dsn_env and no credential_command must be a loud error, got nil")
	}
	if !strings.Contains(err.Error(), "GC_GRAPH_TEST_DSN_MISSING") {
		t.Fatalf("error should name the env var, got %v", err)
	}
}

func TestResolvePostgresDSNNoSourceIsLoud(t *testing.T) {
	cfg := graphJournalBackendConfig{backend: graphJournalBackendPostgres}
	_, err := cfg.resolvePostgresDSN(context.Background())
	if err == nil {
		t.Fatal("resolvePostgresDSN with no dsn_env and no credential_command must be a loud error, got nil")
	}
	if !strings.Contains(err.Error(), "dsn_env") || !strings.Contains(err.Error(), "credential_command") {
		t.Fatalf("error should name both sources, got %v", err)
	}
}

// TestResolvePostgresDSNViaCredentialCommand proves the fake-echo-command → DSN
// path (the hosted BEADS_DOLT_CREDENTIAL_COMMAND analog).
func TestResolvePostgresDSNViaCredentialCommand(t *testing.T) {
	const dsn = "postgres://u:pw@db.example:5432/city?sslmode=disable"
	cfg := graphJournalBackendConfig{
		backend:           graphJournalBackendPostgres,
		credentialCommand: "printf '%s' " + shellSingleQuote(dsn),
	}
	got, err := cfg.resolvePostgresDSN(context.Background())
	if err != nil {
		t.Fatalf("resolvePostgresDSN via credential_command: %v", err)
	}
	if got != dsn {
		t.Fatalf("dsn = %q, want %q", got, dsn)
	}
}

// TestResolvePostgresDSNEnvEmptyFallsThroughToCredentialCommand pins the
// env-override / helper-fallback precedence.
func TestResolvePostgresDSNEnvEmptyFallsThroughToCredentialCommand(t *testing.T) {
	const dsn = "postgres://u:pw@db.example:5432/city"
	t.Setenv("GC_GRAPH_TEST_DSN_EMPTY", "")
	cfg := graphJournalBackendConfig{
		backend:           graphJournalBackendPostgres,
		dsnEnv:            "GC_GRAPH_TEST_DSN_EMPTY",
		credentialCommand: "printf '%s' " + shellSingleQuote(dsn),
	}
	got, err := cfg.resolvePostgresDSN(context.Background())
	if err != nil {
		t.Fatalf("resolvePostgresDSN: %v", err)
	}
	if got != dsn {
		t.Fatalf("dsn = %q, want %q (should fall through to credential_command)", got, dsn)
	}
}

func TestRunGraphJournalCredentialCommandEmptyOutputIsLoud(t *testing.T) {
	_, err := runGraphJournalCredentialCommand(context.Background(), "true") // exits 0, no output
	if err == nil {
		t.Fatal("empty credential_command output must be a loud error, got nil")
	}
	if !strings.Contains(err.Error(), "no output") {
		t.Fatalf("error should explain empty output, got %v", err)
	}
}

func TestRunGraphJournalCredentialCommandNonZeroExitIsLoud(t *testing.T) {
	_, err := runGraphJournalCredentialCommand(context.Background(), "exit 7")
	if err == nil {
		t.Fatal("non-zero credential_command exit must be a loud error, got nil")
	}
}

// TestRunGraphJournalCredentialCommandFailureDoesNotLeakStdoutSecret proves that
// when the helper prints the DSN to stdout and then fails, the secret never
// reaches the error string.
func TestRunGraphJournalCredentialCommandFailureDoesNotLeakStdoutSecret(t *testing.T) {
	const secret = "postgres://u:SUPERSECRET@db/city"
	_, err := runGraphJournalCredentialCommand(context.Background(), "printf '%s' "+shellSingleQuote(secret)+"; exit 1")
	if err == nil {
		t.Fatal("expected a loud error")
	}
	if strings.Contains(err.Error(), "SUPERSECRET") || strings.Contains(err.Error(), secret) {
		t.Fatalf("credential_command error leaked the stdout DSN secret: %v", err)
	}
}

// TestRunGraphJournalCredentialCommandStderrIsRedacted proves a helper that echoes
// a credential to stderr on failure still cannot leak the password.
func TestRunGraphJournalCredentialCommandStderrIsRedacted(t *testing.T) {
	_, err := runGraphJournalCredentialCommand(context.Background(),
		"printf 'auth failed for postgres://u:LEAKEDPW@db/city' 1>&2; exit 2")
	if err == nil {
		t.Fatal("expected a loud error")
	}
	if strings.Contains(err.Error(), "LEAKEDPW") {
		t.Fatalf("credential_command stderr leaked the password: %v", err)
	}
}

// TestGraphJournalBackendByteIdentitySQLite is the #1 invariant: a city with no
// backend selector opens EXACTLY the embedded-SQLite path — the parse resolves to
// sqlite, the journal round-trips a bead, and journal.db is created on disk (never
// a Postgres connection).
func TestGraphJournalBackendByteIdentitySQLite(t *testing.T) {
	cityPath := t.TempDir()
	writeGraphBackendMarker(t, cityPath, "provider: journal\n")

	cfg, err := loadGraphJournalBackendConfig(cityPath)
	if err != nil {
		t.Fatalf("loadGraphJournalBackendConfig: %v", err)
	}
	if cfg.backend != graphJournalBackendSQLite {
		t.Fatalf("backend = %q, want sqlite for a bare provider marker", cfg.backend)
	}

	result, present, err := openCityGraphJournalResultAt(cityPath)
	if err != nil {
		t.Fatalf("openCityGraphJournalResultAt: %v", err)
	}
	if !present || result.Store == nil {
		t.Fatalf("present=%v store=%v, want an opened store", present, result.Store)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(result.Store) })

	created, err := result.Store.Create(beads.Bead{Title: "sqlite-root"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := result.Store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get(%q): %v", created.ID, err)
	}
	if got.Title != "sqlite-root" {
		t.Fatalf("round-trip title = %q, want sqlite-root", got.Title)
	}

	journalDB := filepath.Join(cityPath, ".gc", "graph", "journal.db")
	if _, err := os.Stat(journalDB); err != nil {
		t.Fatalf("SQLite journal.db not created at %q: %v", journalDB, err)
	}
}

// TestOpenCityGraphJournalPostgresUnresolvableDSNIsLoudNoFallback proves the
// no-silent-fallback rule at the opener level without needing a real Postgres: a
// backend=postgres city whose DSN cannot resolve surfaces a loud error AND leaves
// no SQLite journal.db behind (never a split-brain fallback).
func TestOpenCityGraphJournalPostgresUnresolvableDSNIsLoudNoFallback(t *testing.T) {
	cityPath := t.TempDir()
	writeGraphBackendMarker(t, cityPath,
		"backend: postgres\npostgres:\n  dsn_env: GC_GRAPH_TEST_UNRESOLVABLE\n")
	t.Setenv("GC_GRAPH_TEST_UNRESOLVABLE", "") // named but empty ⇒ unresolvable

	result, present, err := openCityGraphJournalResultAt(cityPath)
	if err == nil {
		t.Fatal("a backend=postgres city with an unresolvable DSN must fail loudly, got nil error")
	}
	if !present {
		t.Fatal("present = false; an opted-but-unopenable city must stay opted so the caller surfaces the error")
	}
	if result.Store != nil {
		t.Fatalf("store = %v, want nil on an unresolvable postgres DSN", result.Store)
	}
	journalDB := filepath.Join(cityPath, ".gc", "graph", "journal.db")
	if _, statErr := os.Stat(journalDB); !os.IsNotExist(statErr) {
		t.Fatalf("no SQLite journal.db may be created for a postgres city (no silent fallback); stat err = %v", statErr)
	}
}

// TestFormatCredentialStderrRedactsBeforeTruncating pins the redact-before-truncate
// fix: a helper stderr larger than the 512-byte bound, whose URL-form DSN straddles
// the cut, must not leak any part of the password. Under the old truncate-then-redact
// order the '@' the redaction regex anchors on is cut off and the password prefix
// survives; redact-first scrubs the full DSN before the length bound is applied.
func TestFormatCredentialStderrRedactsBeforeTruncating(t *testing.T) {
	const pw = "SUPERSECRETPASSWORD"
	// 490 bytes of padding pushes the DSN's terminating '@' past byte 512.
	stderr := strings.Repeat("x", 490) + "postgres://user:" + pw + "@db.example/city"
	got := formatCredentialStderr(stderr)
	for _, leak := range []string{pw, "SUPER", "SECRET"} {
		if strings.Contains(got, leak) {
			t.Fatalf("formatCredentialStderr leaked password fragment %q: %q", leak, got)
		}
	}
	if !strings.Contains(got, "xxxxx@") && !strings.Contains(got, "user:xxxxx") {
		t.Fatalf("formatCredentialStderr did not redact the DSN: %q", got)
	}
}

// TestFormatCredentialStderrRuneSafeTruncation proves the length bound never splits
// a multi-byte rune (which would emit invalid UTF-8 into the error string).
func TestFormatCredentialStderrRuneSafeTruncation(t *testing.T) {
	// 520 three-byte runes: forces truncation at a boundary that a byte-index cut
	// could land mid-rune.
	got := formatCredentialStderr(strings.Repeat("界", 520))
	if !utf8.ValidString(got) {
		t.Fatalf("formatCredentialStderr produced invalid UTF-8: %q", got)
	}
}

// TestRunGraphJournalCredentialCommandTimesOut proves a hung helper surfaces a loud
// timeout error rather than wedging the open forever. Before the ctx/timeout fix the
// call had no deadline and would block for the full sleep (an infinite hang in the
// pathological case) — the one failure mode that produces no loud error at all.
func TestRunGraphJournalCredentialCommandTimesOut(t *testing.T) {
	prev := credentialCommandTimeout
	credentialCommandTimeout = 200 * time.Millisecond
	t.Cleanup(func() { credentialCommandTimeout = prev })

	start := time.Now()
	_, err := runGraphJournalCredentialCommand(context.Background(), "sleep 30")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a hung credential_command must fail loudly, got nil error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error should name the timeout, got %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("credential_command did not honor the timeout: elapsed %s", elapsed)
	}
}

// TestRunGraphJournalCredentialCommandHonorsCallerCancel proves an already-canceled
// caller context aborts the helper promptly instead of running it to completion.
func TestRunGraphJournalCredentialCommandHonorsCallerCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := runGraphJournalCredentialCommand(ctx, "sleep 30")
	if err == nil {
		t.Fatal("a canceled context must abort the credential_command, got nil error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("credential_command ignored a canceled context")
	}
}

// TestParseGraphJournalBackendConfigNonMappingIsLoud pins the byte-identity crack
// (MEDIUM e2e): post-P6.4 the marker content is yaml-parsed, so a non-mapping marker
// (a bare scalar, or a sequence) that opened fine pre-P6.4 is now a LOUD error. This
// is the safe choice — silently treating an unparseable marker as the SQLite default
// could mask a corrupted `backend: postgres` and split-brain the journal. An empty or
// comment-only marker still parses to the byte-identical SQLite default.
func TestParseGraphJournalBackendConfigNonMappingIsLoud(t *testing.T) {
	loud := []struct {
		name string
		yaml string
	}{
		{"bare scalar", "journal\n"},
		{"scalar words", "provider journal\n"},
		{"sequence", "- provider\n- journal\n"},
	}
	for _, tc := range loud {
		t.Run(tc.name, func(t *testing.T) {
			if cfg, err := parseGraphJournalBackendConfig([]byte(tc.yaml)); err == nil {
				t.Fatalf("non-mapping marker %q parsed to %+v, want a loud error", tc.yaml, cfg)
			}
		})
	}
	// Byte-identity preserved: empty and comment-only markers stay SQLite.
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{"empty", ""},
		{"comment only", "# just a note\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseGraphJournalBackendConfig([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("parseGraphJournalBackendConfig(%q) error = %v, want SQLite default", tc.yaml, err)
			}
			if cfg.backend != graphJournalBackendSQLite {
				t.Fatalf("backend = %q, want sqlite for %q", cfg.backend, tc.yaml)
			}
		})
	}
}

// TestGraphJournalOpenFailureIsFatal pins the controller-startup classification: a
// postgres-backed or unparseable-marker city's open failure is fatal (never a silent
// degrade to the legacy work store), a confirmed-SQLite city keeps the historical
// warn-and-degrade, and a non-opted city is never fatal.
func TestGraphJournalOpenFailureIsFatal(t *testing.T) {
	t.Run("postgres is fatal", func(t *testing.T) {
		city := t.TempDir()
		writeGraphBackendMarker(t, city, "backend: postgres\npostgres:\n  dsn_env: GC_GRAPH_X\n")
		if !graphJournalOpenFailureIsFatal(city) {
			t.Fatal("a backend=postgres open failure must be fatal (no silent legacy fallback)")
		}
	})
	t.Run("sqlite degrades", func(t *testing.T) {
		city := t.TempDir()
		writeGraphBackendMarker(t, city, "provider: journal\n")
		if graphJournalOpenFailureIsFatal(city) {
			t.Fatal("a confirmed-SQLite open failure must warn-and-degrade, not be fatal")
		}
	})
	t.Run("unparseable marker is fatal", func(t *testing.T) {
		city := t.TempDir()
		writeGraphBackendMarker(t, city, "journal\n") // non-mapping scalar
		if !graphJournalOpenFailureIsFatal(city) {
			t.Fatal("an unparseable marker cannot confirm the SQLite default; must be fatal")
		}
	})
	t.Run("non-opted city is not fatal", func(t *testing.T) {
		if graphJournalOpenFailureIsFatal(t.TempDir()) {
			t.Fatal("a city with no graph scope must never be fatal")
		}
	})
}

// shellSingleQuote wraps s in single quotes for safe use inside an sh -c command,
// escaping any embedded single quotes.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
