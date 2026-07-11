package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pgauth"
)

// bd origin/main's `bd init --backend=postgres` persists metadata.json with a
// password-free postgres_dsn + postgres_schema (NOT discrete
// postgres_host/port/user/database fields) and reads the password at command
// time from BEADS_PG_PASSWORD (beads internal/storage/postgres/credential.go,
// internal/configfile/configfile.go). These tests pin gc's projection against
// that shape.

// writeBdMainPGScopeFixture writes a bd-main-shaped postgres scope: DSN +
// schema metadata plus an optional scope-local password file.
func writeBdMainPGScopeFixture(t *testing.T, scopeRoot, password string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	meta := `{"database":"beads","backend":"postgres","postgres_dsn":"postgres://gc_city@pg.example.test:5432/gascity_infra","postgres_schema":"city_x"}`
	if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	if password != "" {
		envFile := filepath.Join(scopeRoot, ".beads", ".env")
		if err := os.WriteFile(envFile, []byte("BEADS_POSTGRES_PASSWORD="+password+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestApplyResolvedScopePostgresEnv_BdMainDSNScope(t *testing.T) {
	clearAmbientPostgresEnv(t)
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	scopeRoot := t.TempDir()
	writeBdMainPGScopeFixture(t, scopeRoot, "devpw")

	meta, ok, err := contract.LoadMetadataState(fsys.OSFS{}, filepath.Join(scopeRoot, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("LoadMetadataState rejected bd-main postgres_dsn shape: %v", err)
	}
	if !ok {
		t.Fatal("LoadMetadataState ok = false, want true")
	}

	env := map[string]string{}
	if err := applyResolvedScopePostgresEnv(env, cityPath, scopeRoot, meta); err != nil {
		t.Fatalf("applyResolvedScopePostgresEnv: %v", err)
	}
	want := map[string]string{
		"GC_POSTGRES_PASSWORD":    "devpw",
		"BEADS_POSTGRES_PASSWORD": "devpw",
		// bd-main's canonical static-password variable.
		"BEADS_PG_PASSWORD": "devpw",
		// Endpoint derived from the password-free DSN.
		"BEADS_POSTGRES_HOST":     "pg.example.test",
		"BEADS_POSTGRES_PORT":     "5432",
		"BEADS_POSTGRES_USER":     "gc_city",
		"BEADS_POSTGRES_DATABASE": "gascity_infra",
	}
	for key, value := range want {
		if got := env[key]; got != value {
			t.Errorf("env[%q] = %q, want %q", key, got, value)
		}
	}

	// The pg.credential_resolved event must carry the DSN-derived endpoint,
	// not empty strings.
	recorded, err := events.ReadFiltered(
		filepath.Join(cityPath, ".gc", "events.jsonl"),
		events.Filter{Type: events.PostgresCredentialResolved},
	)
	if err != nil {
		t.Fatalf("ReadFiltered pg.credential_resolved: %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("pg.credential_resolved count = %d, want 1", len(recorded))
	}
	var payload pgauth.PostgresCredentialResolvedPayload
	if err := json.Unmarshal(recorded[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Host != "pg.example.test" || payload.Port != "5432" || payload.User != "gc_city" {
		t.Errorf("event endpoint = %s:%s user=%s, want pg.example.test:5432 user=gc_city", payload.Host, payload.Port, payload.User)
	}
}

func TestApplyResolvedScopePostgresEnv_DraftScopeAlsoProjectsBeadsPgPassword(t *testing.T) {
	clearAmbientPostgresEnv(t)
	cityPath := t.TempDir()
	scopeRoot := t.TempDir()
	writePGScopeFixture(t, scopeRoot, "devpw")

	env := map[string]string{}
	meta := contract.MetadataState{
		Backend:          "postgres",
		PostgresHost:     "db.example.test",
		PostgresPort:     "5432",
		PostgresUser:     "bd",
		PostgresDatabase: "beads",
	}
	if err := applyResolvedScopePostgresEnv(env, cityPath, scopeRoot, meta); err != nil {
		t.Fatalf("applyResolvedScopePostgresEnv: %v", err)
	}
	if got := env["BEADS_PG_PASSWORD"]; got != "devpw" {
		t.Errorf(`env["BEADS_PG_PASSWORD"] = %q, want devpw (bd-main reads this variable)`, got)
	}
}

func TestBdRuntimeEnvCity_PostgresBackendBdMainShape(t *testing.T) {
	clearAmbientPostgresEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	writeBdMainPGScopeFixture(t, cityPath, "citypw")
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: city
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env, err := bdRuntimeEnvWithError(cityPath)
	if err != nil {
		t.Fatalf("bdRuntimeEnvWithError() error = %v", err)
	}

	wantPG := map[string]string{
		"GC_POSTGRES_PASSWORD":    "citypw",
		"BEADS_POSTGRES_PASSWORD": "citypw",
		"BEADS_PG_PASSWORD":       "citypw",
		"BEADS_POSTGRES_HOST":     "pg.example.test",
		"BEADS_POSTGRES_PORT":     "5432",
		"BEADS_POSTGRES_USER":     "gc_city",
		"BEADS_POSTGRES_DATABASE": "gascity_infra",
	}
	for key, value := range wantPG {
		if got := env[key]; got != value {
			t.Errorf("env[%q] = %q, want %q", key, got, value)
		}
	}
	for _, key := range projectedDoltEnvKeys {
		if value, ok := env[key]; ok && value != "" {
			t.Errorf("env[%q] = %q, want empty/absent for PG-backed city", key, value)
		}
	}
}

func TestProjectedPostgresEnvKeysIncludeBeadsPgPassword(t *testing.T) {
	for _, key := range projectedPostgresEnvKeys {
		if key == "BEADS_PG_PASSWORD" {
			return
		}
	}
	t.Fatal(`projectedPostgresEnvKeys missing "BEADS_PG_PASSWORD" — dolt scopes would inherit a stale PG password and mergeRuntimeEnv would not scrub it`)
}

func TestAllowLegacyDoltMetadataRepairRejectsPostgresDSN(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".beads", "metadata.json")

	// Control: a bare legacy backend with no postgres fields is repairable.
	if err := os.WriteFile(path, []byte(`{"backend":"legacy"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, loadErr := contract.LoadMetadataState(fs, path)
	if loadErr == nil {
		t.Fatal("LoadMetadataState accepted backend=legacy; test premise broken")
	}
	if !allowLegacyDoltMetadataRepair(fs, path, loadErr) {
		t.Fatal("allowLegacyDoltMetadataRepair = false for bare legacy metadata, want true")
	}

	// A legacy-marked scope that carries a postgres_dsn has postgres
	// metadata and must NOT be treated as a repairable dolt scope.
	if err := os.WriteFile(path, []byte(`{"backend":"legacy","postgres_dsn":"postgres://gc_city@pg.example.test:5432/gascity_infra"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, loadErr = contract.LoadMetadataState(fs, path)
	if loadErr == nil {
		t.Fatal("LoadMetadataState accepted backend=legacy; test premise broken")
	}
	if allowLegacyDoltMetadataRepair(fs, path, loadErr) {
		t.Fatal("allowLegacyDoltMetadataRepair = true for legacy metadata carrying postgres_dsn, want false")
	}
}

// TestBdCommandRunnerDropsEmptyProjectedPGPasswordForPostgresScope pins
// 1d1110b51 (the live pg-infra incident's second half): the internal bd
// command runner builds its env for the CITY scope and reuses it for other
// scopes, so on a Dolt-work city the projected Postgres password reaches a
// metadata-backed pg INFRA scope as an explicit EMPTY string — which bd
// treats as authoritative, skipping its own .beads/.env resolution and
// failing SASL against the external endpoint. The runner must DROP
// explicit-empty projected pg-password keys for a pg-backed scope (so bd's
// own ladder resolves) while keeping them for non-pg scopes (the
// explicit-empty hygiene that guards against inherited stale passwords) and
// passing NON-empty values through untouched. The coverage red-team flagged
// this commit as revert-silent; this test is the invariant.
func TestBdCommandRunnerDropsEmptyProjectedPGPasswordForPostgresScope(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	orig := beadsExecCommandRunnerWithEnv
	defer func() { beadsExecCommandRunnerWithEnv = orig }()
	var captured map[string]string
	beadsExecCommandRunnerWithEnv = func(env map[string]string) beads.CommandRunner {
		captured = env
		return func(_, _ string, _ ...string) ([]byte, error) { return []byte("[]"), nil }
	}

	cityPath := t.TempDir()
	pgScope := t.TempDir()
	writeBdMainPGScopeFixture(t, pgScope, "")
	emptyProjected := func(_ string) (map[string]string, error) {
		return map[string]string{
			"GC_POSTGRES_PASSWORD":    "",
			"BEADS_PG_PASSWORD":       "",
			"BEADS_POSTGRES_PASSWORD": "",
			"BEADS_POSTGRES_HOST":     "pg.example.test",
		}, nil
	}

	if _, err := bdCommandRunnerWithManagedRetryErr(cityPath, emptyProjected)(pgScope, "bd", "list", "--json"); err != nil {
		t.Fatalf("runner on pg scope: %v", err)
	}
	for _, k := range []string{"GC_POSTGRES_PASSWORD", "BEADS_PG_PASSWORD", "BEADS_POSTGRES_PASSWORD"} {
		if v, present := captured[k]; present {
			t.Errorf("pg scope: explicit-empty %s=%q must be DROPPED so bd's .beads/.env tier resolves", k, v)
		}
	}
	if captured["BEADS_POSTGRES_HOST"] != "pg.example.test" {
		t.Errorf("pg scope: non-password key must survive, got %q", captured["BEADS_POSTGRES_HOST"])
	}

	// A NON-empty projected password passes through untouched on a pg scope.
	if _, err := bdCommandRunnerWithManagedRetryErr(cityPath, func(_ string) (map[string]string, error) {
		return map[string]string{"BEADS_PG_PASSWORD": "real-secret"}, nil
	})(pgScope, "bd", "list", "--json"); err != nil {
		t.Fatalf("runner with real password: %v", err)
	}
	if captured["BEADS_PG_PASSWORD"] != "real-secret" {
		t.Errorf("pg scope: non-empty password must pass through, got %q", captured["BEADS_PG_PASSWORD"])
	}

	// A NON-pg scope keeps the explicit-empty keys (the stale-password guard).
	plainScope := t.TempDir()
	if _, err := bdCommandRunnerWithManagedRetryErr(cityPath, emptyProjected)(plainScope, "bd", "list", "--json"); err != nil {
		t.Fatalf("runner on plain scope: %v", err)
	}
	for _, k := range []string{"GC_POSTGRES_PASSWORD", "BEADS_PG_PASSWORD", "BEADS_POSTGRES_PASSWORD"} {
		if _, present := captured[k]; !present {
			t.Errorf("non-pg scope: explicit-empty %s must be KEPT (inherited-password hygiene)", k)
		}
	}
}
