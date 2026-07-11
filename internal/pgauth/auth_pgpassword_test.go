package pgauth

import (
	"testing"
)

// bd origin/main reads the static Postgres password from BEADS_PG_PASSWORD
// (beads internal/storage/postgres/credential.go). These tests pin the
// resolver's recognition of that canonical variable alongside the legacy
// BEADS_POSTGRES_PASSWORD / GC_POSTGRES_PASSWORD names.

func TestResolveFromEnv_ProjectedBeadsPg(t *testing.T) {
	clearProcessEnv(t)
	envMap := map[string]string{
		"BEADS_PG_PASSWORD": "pg-main-secret",
	}
	t.Setenv("GC_POSTGRES_PASSWORD", "process-gc-loses")

	got, err := ResolveFromEnv(envMap, t.TempDir(), endpoint())
	if err != nil {
		t.Fatalf("ResolveFromEnv error: %v", err)
	}
	if got.Password != "pg-main-secret" {
		t.Fatalf("Password = %q, want pg-main-secret", got.Password)
	}
	if got.Source != SourceProjectedBeadsPg {
		t.Fatalf("Source = %v, want SourceProjectedBeadsPg", got.Source)
	}
}

func TestResolveFromEnv_ProjectedBeadsOutranksProjectedBeadsPg(t *testing.T) {
	clearProcessEnv(t)
	envMap := map[string]string{
		"BEADS_POSTGRES_PASSWORD": "legacy-wins",
		"BEADS_PG_PASSWORD":       "pg-main-loses",
	}

	got, err := ResolveFromEnv(envMap, t.TempDir(), endpoint())
	if err != nil {
		t.Fatalf("ResolveFromEnv error: %v", err)
	}
	if got.Password != "legacy-wins" {
		t.Fatalf("Password = %q, want legacy-wins", got.Password)
	}
	if got.Source != SourceProjectedBeads {
		t.Fatalf("Source = %v, want SourceProjectedBeads", got.Source)
	}
}

func TestResolveFromEnv_ProcessEnvBeadsPg(t *testing.T) {
	clearProcessEnv(t)
	t.Setenv("BEADS_PG_PASSWORD", "pg-main-process")
	credPath := writeCredentialsFile(t, "127.0.0.1", "5433", "credfile-loses")
	t.Setenv("BEADS_CREDENTIALS_FILE", credPath)

	got, err := ResolveFromEnv(nil, t.TempDir(), endpoint())
	if err != nil {
		t.Fatalf("ResolveFromEnv error: %v", err)
	}
	if got.Password != "pg-main-process" {
		t.Fatalf("Password = %q, want pg-main-process", got.Password)
	}
	if got.Source != SourceProcessEnvBeadsPg {
		t.Fatalf("Source = %v, want SourceProcessEnvBeadsPg", got.Source)
	}
}

func TestResolveFromEnv_ProcessEnvBeadsOutranksProcessEnvBeadsPg(t *testing.T) {
	clearProcessEnv(t)
	t.Setenv("BEADS_POSTGRES_PASSWORD", "legacy-process-wins")
	t.Setenv("BEADS_PG_PASSWORD", "pg-main-loses")

	got, err := ResolveFromEnv(nil, t.TempDir(), endpoint())
	if err != nil {
		t.Fatalf("ResolveFromEnv error: %v", err)
	}
	if got.Password != "legacy-process-wins" {
		t.Fatalf("Password = %q, want legacy-process-wins", got.Password)
	}
	if got.Source != SourceProcessEnvBeads {
		t.Fatalf("Source = %v, want SourceProcessEnvBeads", got.Source)
	}
}

func TestResolveFromEnv_ScopeFileOutranksProcessEnvBeadsPg(t *testing.T) {
	clearProcessEnv(t)
	scopeRoot := t.TempDir()
	writeStorePassword(t, scopeRoot, "scope-file-wins")
	t.Setenv("BEADS_PG_PASSWORD", "pg-main-loses")

	got, err := ResolveFromEnv(nil, scopeRoot, endpoint())
	if err != nil {
		t.Fatalf("ResolveFromEnv error: %v", err)
	}
	if got.Password != "scope-file-wins" {
		t.Fatalf("Password = %q, want scope-file-wins", got.Password)
	}
	if got.Source != SourceScopeFile {
		t.Fatalf("Source = %v, want SourceScopeFile", got.Source)
	}
}

func TestSourceString_BeadsPgIdentifiers(t *testing.T) {
	if got := SourceProjectedBeadsPg.String(); got != "projected_beads_pg" {
		t.Errorf("SourceProjectedBeadsPg.String() = %q, want projected_beads_pg", got)
	}
	if got := SourceProcessEnvBeadsPg.String(); got != "process_env_beads_pg" {
		t.Errorf("SourceProcessEnvBeadsPg.String() = %q, want process_env_beads_pg", got)
	}
}
