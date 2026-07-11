package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// writeBdMainPGMetadata writes a bd-origin/main-shaped postgres scope:
// password-free postgres_dsn + postgres_schema, no discrete
// postgres_host/port/user/database fields.
func writeBdMainPGMetadata(t *testing.T, scopeRoot string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"database":"beads","backend":"postgres","postgres_dsn":"postgres://gc_city@pg.example.test:5432/gascity_infra","postgres_schema":"city_x"}`
	if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPostgresAuthCheck_BdMainDSNScope pins the doctor against bd
// origin/main's metadata shape: the scope must be collected (not silently
// filtered) and the probe endpoint must be derived from the DSN so the
// credentials-file tiers key on the real [host:port].
func TestPostgresAuthCheck_BdMainDSNScope(t *testing.T) {
	scrubAmbientPostgresEnv(t)
	cityPath := t.TempDir()
	writeBdMainPGMetadata(t, cityPath)
	writePGScopeEnv(t, cityPath)

	check := NewPostgresAuthCheck(cityPath, &config.City{})
	r := check.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusOK {
		t.Fatalf("status = %v; want StatusOK; message=%q", r.Status, r.Message)
	}
	if want := "city (pg.example.test:5432): password from scope file"; r.Message != want {
		t.Fatalf("message = %q; want %q (endpoint must be derived from postgres_dsn)", r.Message, want)
	}
}
