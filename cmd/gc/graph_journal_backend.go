package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/processgroup"
	"gopkg.in/yaml.v3"
)

// graphJournalBackend names the storage engine a city's graph journal opens on.
type graphJournalBackend string

const (
	// graphJournalBackendSQLite is the default embedded-SQLite engine — the
	// byte-identical pre-P6.4 path. Selected when the marker has no `backend` key
	// or `backend: sqlite`.
	graphJournalBackendSQLite graphJournalBackend = "sqlite"
	// graphJournalBackendPostgres is the hosted-durability engine
	// (graphstore.OpenPostgres). Selected by `backend: postgres`, which additionally
	// requires a DSN source under `postgres:`.
	graphJournalBackendPostgres graphJournalBackend = "postgres"
)

// graphJournalConfigFile is the parsed shape of
// <city>/.gc/graph/.beads/config.yaml. The mere presence of the file opts the
// city into the graph-journal scope (the historical contract that
// cityGraphScopePresence keys on); the fields here are an OPTIONAL backend
// selector layered on top:
//
//	provider: journal        # historical, human-legible tag (not routing-load-bearing)
//	backend: postgres        # optional: sqlite (default/absent) | postgres
//	postgres:                # meaningful only when backend: postgres
//	  dsn_env: GC_GRAPH_PG_DSN            # env var whose value is the DSN
//	  credential_command: eia-graph-cred # command whose STDOUT is the DSN
//
// An absent file, an absent `backend`, or `backend: sqlite` all resolve to the
// byte-identical embedded-SQLite path. Unknown keys are ignored (yaml.v3 default),
// so an existing `provider: journal` marker parses to the SQLite default with no
// behavior change.
//
// SECURITY FOOTGUN — the marker travels WITH the city directory. Unlike git's
// repo-local config (.git/config is deliberately NOT transported by clone so a
// hostile repo cannot ship a credential.helper), this file lives inside
// .gc/graph/.beads and IS carried by a tarball/zip of a city or by a repo whose
// author tracks .gc/. A `postgres.credential_command` here is therefore
// attacker-supplied when you open an untrusted city, and it runs on ordinary
// read/dispatch paths (see runGraphJournalCredentialCommand). `gc migrate
// graph-journal init` only ever writes `provider: journal` — the postgres stanza
// is operator-hand-edited — so a postgres marker in a downloaded city is a signal
// to audit before running any gc command against it.
type graphJournalConfigFile struct {
	Provider string                     `yaml:"provider"`
	Backend  string                     `yaml:"backend"`
	Postgres graphJournalPostgresConfig `yaml:"postgres"`
}

// graphJournalPostgresConfig sources the Postgres DSN without ever writing the
// secret to disk. At open time exactly one source yields the DSN:
//
//   - DSNEnv names an environment variable whose value is the DSN.
//   - CredentialCommand is a command whose STDOUT is the DSN, mirroring the hosted
//     BEADS_DOLT_CREDENTIAL_COMMAND pattern — the credential lives only in the
//     helper→gc pipe, never in the config file committed to the city.
//
// When both are set, DSNEnv wins if its variable is non-empty; otherwise gc falls
// through to CredentialCommand (env override, helper fallback).
type graphJournalPostgresConfig struct {
	DSNEnv            string `yaml:"dsn_env"`
	CredentialCommand string `yaml:"credential_command"`
}

// graphJournalBackendConfig is the resolved, validated backend selection for a
// city's graph journal.
type graphJournalBackendConfig struct {
	backend           graphJournalBackend
	dsnEnv            string
	credentialCommand string
}

// loadGraphJournalBackendConfig reads and validates the backend selector from a
// city's graph-scope marker. A city carrying only the historical
// `provider: journal` marker (no `backend`) resolves to SQLite — the
// byte-identical default. A malformed marker or an unsupported backend is a loud
// error: this path never silently degrades a hosted city to embedded SQLite.
func loadGraphJournalBackendConfig(cityPath string) (graphJournalBackendConfig, error) {
	path := filepath.Join(graphScopeRoot(cityPath), ".beads", "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return graphJournalBackendConfig{}, fmt.Errorf("reading graph journal config %q: %w", path, err)
	}
	cfg, err := parseGraphJournalBackendConfig(data)
	if err != nil {
		return graphJournalBackendConfig{}, fmt.Errorf("parsing graph journal config %q: %w", path, err)
	}
	return cfg, nil
}

// parseGraphJournalBackendConfig parses the marker bytes into a validated backend
// selection. Absent/empty/`sqlite` backend → SQLite default; `postgres` → the
// Postgres selection with its DSN sources trimmed; anything else is a loud error.
func parseGraphJournalBackendConfig(data []byte) (graphJournalBackendConfig, error) {
	var raw graphJournalConfigFile
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return graphJournalBackendConfig{}, err
	}
	switch backend := strings.TrimSpace(raw.Backend); backend {
	case "", string(graphJournalBackendSQLite):
		return graphJournalBackendConfig{backend: graphJournalBackendSQLite}, nil
	case string(graphJournalBackendPostgres):
		return graphJournalBackendConfig{
			backend:           graphJournalBackendPostgres,
			dsnEnv:            strings.TrimSpace(raw.Postgres.DSNEnv),
			credentialCommand: strings.TrimSpace(raw.Postgres.CredentialCommand),
		}, nil
	default:
		return graphJournalBackendConfig{}, fmt.Errorf("unsupported graph journal backend %q (supported: sqlite, postgres)", backend)
	}
}

// openGraphStore opens the city's journal graph engine on the configured backend.
//
// The SQLite branch is byte-identical to the pre-P6.4 opener (the same
// graphstore.Open call on the same journal.db path). The Postgres branch resolves
// the DSN and opens the write-ready Postgres engine; a Postgres backend whose DSN
// cannot be resolved is a loud, terminal error and NEVER falls back to SQLite —
// a silent fallback would split-brain the journal across two engines.
func (c graphJournalBackendConfig) openGraphStore(ctx context.Context, cityPath string) (*graphstore.Store, error) {
	opts := graphstore.Options{CityID: graphScopeCityID(cityPath)}
	switch c.backend {
	case graphJournalBackendSQLite:
		return graphstore.Open(ctx, filepath.Join(graphScopeRoot(cityPath), "journal.db"), opts)
	case graphJournalBackendPostgres:
		dsn, err := c.resolvePostgresDSN(ctx)
		if err != nil {
			return nil, err
		}
		return graphstore.OpenPostgres(ctx, dsn, opts)
	default:
		return nil, fmt.Errorf("graph journal backend %q is not openable", c.backend)
	}
}

// resolvePostgresDSN resolves the Postgres DSN from the configured sources. It
// NEVER returns the DSN inside an error and NEVER logs it; a configured-but-
// unresolvable DSN is a loud, terminal error so the caller can refuse rather than
// fall back to SQLite. ctx bounds the credential_command arm so a hung helper
// cannot wedge the open indefinitely.
func (c graphJournalBackendConfig) resolvePostgresDSN(ctx context.Context) (string, error) {
	if env := strings.TrimSpace(c.dsnEnv); env != "" {
		if dsn := strings.TrimSpace(os.Getenv(env)); dsn != "" {
			return dsn, nil
		}
		// The env var is named but unset/empty. Fall through to the credential
		// command when one is configured; otherwise fail loudly, naming the env
		// variable (never its contents).
		if strings.TrimSpace(c.credentialCommand) == "" {
			return "", fmt.Errorf("graph journal backend=postgres: dsn_env %q is unset or empty and no credential_command is configured", env)
		}
	}
	if command := strings.TrimSpace(c.credentialCommand); command != "" {
		return runGraphJournalCredentialCommand(ctx, command)
	}
	return "", fmt.Errorf("graph journal backend=postgres requires a DSN source: set postgres.dsn_env or postgres.credential_command in .gc/graph/.beads/config.yaml")
}

// graphJournalOpenFailureIsFatal reports whether a failed open of the city graph
// journal must abort controller startup rather than silently degrade graph-class
// routing to the legacy work store. A backend=postgres city — or one whose marker
// cannot be parsed to CONFIRM the byte-identical SQLite default — risks a
// cross-backend split-brain if graph-class writes land in the work store while
// journal-resident beads live in Postgres, so its open failure is terminal. A
// confirmed-SQLite city keeps the historical warn-and-degrade (same-host,
// byte-identical to pre-P6.4). An absent marker is a non-opted city, which
// degrades correctly by construction and is never fatal.
func graphJournalOpenFailureIsFatal(cityPath string) bool {
	cfg, err := loadGraphJournalBackendConfig(cityPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No marker: not opted in. Routing graph beads to the work store is the
			// byte-identical non-opted path, never a split-brain.
			return false
		}
		// Marker present but unparseable/unreadable: we cannot confirm the SQLite
		// default, and it may be a corrupted `backend: postgres`. Refuse to guess —
		// treat as fatal so postgres-resident beads never silently route to legacy.
		return true
	}
	return cfg.backend == graphJournalBackendPostgres
}

// credentialCommandTimeout bounds a credential helper so a hung helper (a network
// call to a token service, or a prompt that opens /dev/tty) can never wedge
// controller startup or a one-shot gc command forever. It is applied on top of the
// caller's ctx, so an earlier caller deadline still wins. Overridable in tests.
var credentialCommandTimeout = 30 * time.Second

// runGraphJournalCredentialCommand runs the operator-configured credential command
// and returns its stdout as the DSN, mirroring the hosted BEADS_DOLT_CREDENTIAL_COMMAND
// contract: the command mints the DSN at runtime so the secret never lands on disk.
//
// Security / robustness posture:
//   - stdout is the SECRET (the DSN). It is captured and returned but NEVER logged
//     and NEVER placed in an error.
//   - the command is a config value that travels WITH the city directory (unlike
//     git's credential.helper, which lives in the non-transported .git/config), so
//     it is only as trustworthy as the city you point gc at. It is run through the
//     shell exactly as written with NO interpolation of untrusted data (no injection
//     surface), but running gc against an UNTRUSTED city executes this command — see
//     the footgun note on graphJournalConfigFile.
//   - the helper is bounded by ctx + credentialCommandTimeout: a hang becomes a
//     clear timeout error, never the silent infinite wait that would produce no
//     loud error at all.
//   - a non-zero exit or empty stdout is a loud, terminal error (never a silent
//     empty DSN that would then be mis-resolved).
func runGraphJournalCredentialCommand(ctx context.Context, command string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, credentialCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	// Run the helper in its own process group and tear the WHOLE group down on
	// cancel: a bare Process.Kill leaves an inherited child (the classic `sh -c` →
	// child case) holding the stdout/stderr pipe, so cmd.Wait would block until that
	// child exits — reintroducing the very hang the timeout exists to prevent. The
	// WaitDelay backstop force-closes the pipes if the group somehow lingers.
	processgroup.StartCommandInNewGroup(cmd)
	cmd.Cancel = func() error {
		return processgroup.TerminateCommand(cmd, 0, shellExecSignalGrace, processgroup.Options{})
	}
	cmd.WaitDelay = shellExecPostCancelWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// The context firing (deadline/cancel) kills the helper; report it as such
		// rather than as an opaque "signal: killed" exit.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("graph journal credential_command timed out after %s", credentialCommandTimeout)
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return "", fmt.Errorf("graph journal credential_command canceled before completion: %w", ctx.Err())
		}
		// Deliberately omit stdout (it may hold a partial secret). stderr is the
		// helper's diagnostic channel; include a bounded, redacted slice of it.
		return "", fmt.Errorf("graph journal credential_command failed: %w%s", err, formatCredentialStderr(stderr.String()))
	}
	dsn := strings.TrimSpace(stdout.String())
	if dsn == "" {
		return "", fmt.Errorf("graph journal credential_command produced no output (expected a Postgres DSN on stdout)")
	}
	return dsn, nil
}

// formatCredentialStderr renders a credential command's stderr for an error
// message: trimmed, redacted through graphstore.RedactDSN, and only THEN
// length-bounded so a misbehaving helper that echoes the DSN to stderr cannot leak
// it. Order matters: graphstore.RedactDSN's URL-userinfo pattern anchors on the
// terminating '@', so truncating first could cut a URL-form DSN before its '@' and
// leave the password prefix unredacted. Redaction is the identity on
// credential-free text, so redact-then-truncate is free on the common path.
func formatCredentialStderr(stderr string) string {
	trimmed := strings.TrimSpace(stderr)
	if trimmed == "" {
		return ""
	}
	redacted := graphstore.RedactDSN(trimmed)
	const maxStderr = 512
	if len(redacted) > maxStderr {
		redacted = truncateBytesRuneSafe(redacted, maxStderr) + "…"
	}
	return " (stderr: " + redacted + ")"
}

// truncateBytesRuneSafe returns s bounded to at most maxBytes bytes without
// splitting a multi-byte UTF-8 rune at the cut.
func truncateBytesRuneSafe(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
