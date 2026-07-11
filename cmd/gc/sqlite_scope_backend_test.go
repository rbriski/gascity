package main

// A split city can host its INFRA store on bd's sqlite backend
// (`bd init --backend=sqlite` on beads origin/main). gc never manages sqlite
// storage — no server, no credentials, no Dolt machinery — so every
// "is this scope non-dolt" branch that already skips Dolt machinery for
// postgres scopes must give the same answer for sqlite scopes. These tests
// pin that behavior across the init, env-projection, order-exec, and
// claim-routing seams. The metadata fixture matches what bd's
// runInitSQLite + configfile.Save author: {database, backend:"sqlite",
// sqlite_path, project_id}.

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// sqliteScopeMetadataJSON is the exact .beads/metadata.json shape
// `bd init --backend=sqlite` writes (beads origin/main runInitSQLite →
// configfile.Save): backend selection plus the file-relative sqlite path
// and project identity. No dolt_* or postgres_* fields.
const sqliteScopeMetadataJSON = `{
  "database": "beads.db",
  "backend": "sqlite",
  "sqlite_path": "beads.db",
  "project_id": "bd-sqlite-test-project"
}`

func writeSQLiteScopeFixture(t *testing.T, scopeRoot string) (metadataPath, configPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	configPath = filepath.Join(scopeRoot, ".beads", "config.yaml")
	// The gc-seeded infra scope carries the rig-shaped inherited-city
	// endpoint state; a bd re-init to sqlite replaces metadata.json but
	// leaves this config.yaml in place.
	if err := os.WriteFile(configPath, []byte("issue_prefix: "+config.InfraScopePrefix+"\ngc.endpoint_origin: inherited_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	metadataPath = filepath.Join(scopeRoot, ".beads", "metadata.json")
	if err := os.WriteFile(metadataPath, []byte(sqliteScopeMetadataJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	return metadataPath, configPath
}

// writeExternalCanonicalCityFixture writes a city whose WORK store is Dolt
// with a pinned external endpoint (city_canonical), so city-level env
// construction succeeds without a live managed Dolt server.
func writeExternalCanonicalCityFixture(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: gc\ngc.endpoint_origin: city_canonical\ngc.endpoint_status: verified\ndolt.host: city-db.example.test\ndolt.port: 4406\ndolt.user: city-user\ndolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=city-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

func installProviderCallProbe(t *testing.T, cityPath string) (callsFile string) {
	t.Helper()
	callsFile = filepath.Join(t.TempDir(), "provider-calls.log")
	script := filepath.Join(t.TempDir(), "gc-beads-bd")
	scriptBody := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s
' "$*" >> %q
exit 99
`, callsFile)
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	return callsFile
}

func TestInitAndHookDirPreservesSQLiteMetadataAndSkipsDoltInit(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: gc\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(cityPath, ".beads", "metadata.json")
	if err := os.WriteFile(metadataPath, []byte(sqliteScopeMetadataJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	callsFile := installProviderCallProbe(t, cityPath)

	if err := initAndHookDir(cityPath, cityPath, "gc"); err != nil {
		t.Fatalf("initAndHookDir: %v", err)
	}
	if data, err := os.ReadFile(callsFile); err == nil {
		t.Fatalf("provider init must not run for sqlite metadata; calls:\n%s", data)
	} else if !os.IsNotExist(err) {
		t.Fatalf("read provider calls: %v", err)
	}
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if string(data) != sqliteScopeMetadataJSON {
		t.Fatalf("metadata = %s, want preserved sqlite metadata %s", data, sqliteScopeMetadataJSON)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".beads", "hooks", "on_create")); !os.IsNotExist(err) {
		t.Fatalf("gc must not install bd event hooks for sqlite scope (stat err=%v)", err)
	}
}

func TestInitAndHookDirSkipsDoltInitForSQLiteInfraScope(t *testing.T) {
	// Deploy shape: Dolt WORK store (managed city) + sqlite-backend INFRA
	// store at .gc/infra. Startup runs initAndHookDir against the infra
	// scope; it must be hooks-only — never the Dolt bd-init machinery that
	// would clobber bd's sqlite-authored config.
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: gc\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	infraDir := infraScopeRoot(cityPath)
	metadataPath, configPath := writeSQLiteScopeFixture(t, infraDir)
	configBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	callsFile := installProviderCallProbe(t, cityPath)

	if err := initAndHookDir(cityPath, infraDir, config.InfraScopePrefix); err != nil {
		t.Fatalf("initAndHookDir: %v", err)
	}
	if data, err := os.ReadFile(callsFile); err == nil {
		t.Fatalf("provider init must not run for sqlite infra scope; calls:\n%s", data)
	} else if !os.IsNotExist(err) {
		t.Fatalf("read provider calls: %v", err)
	}
	metaAfter, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if string(metaAfter) != sqliteScopeMetadataJSON {
		t.Fatalf("metadata = %s, want preserved sqlite metadata", metaAfter)
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(configAfter) != string(configBefore) {
		t.Fatalf("config.yaml rewritten for sqlite scope:\nbefore: %s\nafter: %s", configBefore, configAfter)
	}
}

func TestScopeUsesNonDoltBackendForInitClassifiesBackends(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata string // empty means no metadata.json
		want     bool
	}{
		{name: "sqlite_skips_dolt_init", metadata: sqliteScopeMetadataJSON, want: true},
		{name: "postgres_skips_dolt_init", metadata: `{"database":"beads","backend":"postgres","postgres_host":"db.example.test","postgres_port":"5432","postgres_user":"bd","postgres_database":"beads_pg"}`, want: true},
		{name: "dolt_takes_dolt_init", metadata: `{"database":"beads","backend":"dolt","dolt_database":"hq"}`, want: false},
		{name: "absent_metadata_takes_dolt_init", metadata: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: gc\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.metadata != "" {
				if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(tc.metadata), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := scopeUsesNonDoltBackendForInit(cityPath, cityPath)
			if err != nil {
				t.Fatalf("scopeUsesNonDoltBackendForInit() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("scopeUsesNonDoltBackendForInit() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApplyOrderExecManagedDoltFallbackSkipsSQLiteScope(t *testing.T) {
	clearAmbientPostgresEnv(t)
	cityPath := writeManagedDoltCityFixture(t)
	scopeRoot := filepath.Join(cityPath, ".gc", "infra")
	writeSQLiteScopeFixture(t, scopeRoot)

	env := map[string]string{}
	if applyOrderExecManagedDoltFallback(cityPath, scopeRoot, env, fmt.Errorf("simulated target error")) {
		t.Fatal("managed Dolt fallback applied to sqlite scope, want skipped")
	}
	if len(env) != 0 {
		t.Fatalf("env = %v, want untouched for sqlite scope", env)
	}
}

func TestPostgresMetadataForScopeSQLiteBackendIsNotPostgres(t *testing.T) {
	cityPath := writeExternalCanonicalCityFixture(t)
	scopeRoot := filepath.Join(cityPath, ".gc", "infra")
	writeSQLiteScopeFixture(t, scopeRoot)

	_, ok, err := postgresMetadataForScope(cityPath, scopeRoot)
	if err != nil {
		t.Fatalf("postgresMetadataForScope() error = %v, want nil for sqlite scope (not postgres, not an error)", err)
	}
	if ok {
		t.Fatal("postgresMetadataForScope() ok = true, want false for sqlite scope")
	}
	if scopeBackendIsPostgres(cityPath, scopeRoot) {
		t.Fatal("scopeBackendIsPostgres() = true, want false for sqlite scope")
	}
}

// writeManagedDoltCityFixture writes a city whose WORK store is a
// gc-managed local Dolt server (the maintainer-city shape) with a published
// runtime state, so managed-endpoint resolution succeeds.
func writeManagedDoltCityFixture(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: gc\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	if err := writeDoltState(cityPath, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: "2026-07-01T08:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

func TestApplyOrderExecCanonicalDoltEnvSkipsSQLiteScope(t *testing.T) {
	clearAmbientPostgresEnv(t)
	cityPath := writeManagedDoltCityFixture(t)
	scopeRoot := filepath.Join(cityPath, ".gc", "infra")
	writeSQLiteScopeFixture(t, scopeRoot)

	env := map[string]string{}
	applyOrderExecCanonicalDoltEnv(cityPath, scopeRoot, env)
	if got, ok := env["GC_DOLT_PORT"]; ok {
		t.Fatalf("GC_DOLT_PORT = %q, want no Dolt endpoint projected onto sqlite scope", got)
	}
	if got, ok := env["GC_DOLT_MANAGED_LOCAL"]; ok {
		t.Fatalf("GC_DOLT_MANAGED_LOCAL = %q, want unset for sqlite scope", got)
	}
	if len(env) != 0 {
		t.Fatalf("env = %v, want untouched for sqlite scope", env)
	}
}

func TestBdRuntimeEnvForRigSQLiteScopeSkipsDoltProjection(t *testing.T) {
	clearAmbientPostgresEnv(t)
	cityPath := writeExternalCanonicalCityFixture(t)
	scopeRoot := filepath.Join(cityPath, ".gc", "infra")
	writeSQLiteScopeFixture(t, scopeRoot)

	env, err := bdRuntimeEnvForRigWithError(cityPath, &config.City{}, scopeRoot)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithError() error = %v, want nil for sqlite scope", err)
	}
	if got := env["BEADS_DIR"]; got != filepath.Join(scopeRoot, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", got, filepath.Join(scopeRoot, ".beads"))
	}
	for _, key := range []string{
		"GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_PASSWORD",
		"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PASSWORD",
		"BEADS_POSTGRES_HOST", "BEADS_POSTGRES_PASSWORD",
	} {
		if got := env[key]; got != "" {
			t.Fatalf("%s = %q, want empty: sqlite scope needs only BEADS_DIR (no server, no creds)", key, got)
		}
	}
}

func TestOpenCityInfraStoreSQLiteScopeFallsBackToBdStore(t *testing.T) {
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not in PATH")
	}
	clearAmbientPostgresEnv(t)
	cityPath := writeExternalCanonicalCityFixture(t)
	infraDir := infraScopeRoot(cityPath)
	writeSQLiteScopeFixture(t, infraDir)

	result, present, err := openCityInfraStoreResultAt(cityPath)
	if err != nil {
		t.Fatalf("openCityInfraStoreResultAt() error = %v, want nil (BdStore fallback is backend-transparent)", err)
	}
	if !present {
		t.Fatal("present = false, want true (infra scope exists)")
	}
	if result.Store == nil {
		t.Fatal("store = nil, want bd-shelling store")
	}
	if result.Diagnostic.Store != beads.BeadsStoreNameBdStore {
		t.Fatalf("diagnostic store = %q, want %q (native Dolt store must decline a sqlite scope)", result.Diagnostic.Store, beads.BeadsStoreNameBdStore)
	}
}

func TestHookClaimInfraDirEnvRoutesReservedClaimToSQLiteInfraScope(t *testing.T) {
	clearAmbientPostgresEnv(t)
	cityPath := writeExternalCanonicalCityFixture(t)
	infraDir := infraScopeRoot(cityPath)
	writeSQLiteScopeFixture(t, infraDir)

	workDir := filepath.Join(cityPath, "rigs", "work")
	baseEnv := []string{"KEEP=1"}
	dir, env := hookClaimInfraDirEnv(cityPath, &config.City{}, config.InfraScopePrefix+"-abc12", workDir, baseEnv)
	if dir != infraDir {
		t.Fatalf("claim dir = %q, want infra scope %q (fell back to work store: infra env resolution failed)", dir, infraDir)
	}
	foundBeadsDir := false
	wantBeadsDir := "BEADS_DIR=" + filepath.Join(infraDir, ".beads")
	for _, entry := range env {
		if entry == wantBeadsDir {
			foundBeadsDir = true
		}
		key, value, _ := strings.Cut(entry, "=")
		switch key {
		case "GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PASSWORD":
			if value != "" {
				t.Fatalf("claim env projects %s=%q onto sqlite infra scope, want no Dolt endpoint", key, value)
			}
		}
	}
	if !foundBeadsDir {
		t.Fatalf("claim env missing %q; env = %v", wantBeadsDir, env)
	}
}
