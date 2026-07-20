// Package dolt_test validates the shared shell helpers in
// assets/scripts/runtime.sh that reclaim server-side work orphaned by a
// client-side run_bounded timeout (gascity ga-tyg). On Dolt 2.1.10,
// run_bounded's --kill-after only terminates the client CLI; the
// server-side CALL (DOLT_FETCH/DOLT_PUSH/DOLT_GC/...) keeps running to
// completion, which left calls accumulating for hours. These tests cover
// sql_escape_literal (safe SQL string-literal embedding),
// dolt_maintenance_lock_key (the lock-file basename shared by `gc dolt
// compact` and `gc dolt sync`), and dolt_kill_stale_queries (the
// processlist scan + KILL QUERY reclaim).
package dolt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const runtimeScript = "assets/scripts/runtime.sh"

// runRuntimeFunc sources runtime.sh against a throwaway city directory, then
// invokes the named shell function with args, returning combined output.
func runRuntimeFunc(t *testing.T, extraEnv []string, funcName string, args ...string) string {
	t.Helper()
	root := repoRoot(t)
	cityPath := t.TempDir()

	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
	}
	script := ". " + shQuote(filepath.Join(root, runtimeScript)) + "\n" +
		funcName + " " + strings.Join(quoted, " ") + "\n"

	cmd := exec.Command("sh", "-c", script)
	// runtime.sh resolves GC_DOLT_PORT unconditionally on source, even for
	// helpers (sql_escape_literal, dolt_maintenance_lock_key,
	// dolt_kill_stale_queries) that never touch the port. Seed a fixed
	// operator-provided port so resolution succeeds without a live managed
	// runtime state file in the throwaway city dir.
	cmd.Env = append(filteredEnv(), "GC_CITY_PATH="+cityPath, "GC_PACK_DIR="+root, "GC_DOLT_PORT=3307")
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("runRuntimeFunc %s: %v\noutput:\n%s", funcName, err, out)
	}
	return string(out)
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func TestSQLEscapeLiteralDoublesBackslashesAndQuotes(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"plain", "hello", "hello"},
		{"single-quote", "O'Brien", "O''Brien"},
		{"backslash", `a\b`, `a\\b`},
		{"backslash-then-quote", `a\'b`, `a\\''b`},
		{"sql-injection-attempt", "x' OR '1'='1", "x'' OR ''1''=''1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := runRuntimeFunc(t, nil, "sql_escape_literal", tc.input)
			got := strings.TrimSuffix(out, "\n")
			if got != tc.want {
				t.Errorf("sql_escape_literal(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestDoltMaintenanceLockKeyCollapsesLoopbackAliases(t *testing.T) {
	cases := []struct {
		name string
		host string
		port string
		want string
	}{
		{"empty", "", "3307", "127.0.0.1-3307"},
		{"localhost", "localhost", "3307", "127.0.0.1-3307"},
		{"unspecified", "0.0.0.0", "3307", "127.0.0.1-3307"},
		{"loopback-ip", "127.0.0.1", "3307", "127.0.0.1-3307"},
		{"loopback-alt", "127.1.2.3", "3307", "127.0.0.1-3307"},
		{"ipv6-loopback", "::1", "3307", "127.0.0.1-3307"},
		{"external-host", "dolt.internal.example", "3307", "dolt.internal.example-3307"},
		{"uppercase", "DOLT.EXAMPLE", "3307", "dolt.example-3307"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := runRuntimeFunc(t, nil, "dolt_maintenance_lock_key", tc.host, tc.port)
			got := strings.TrimSuffix(out, "\n")
			if got != tc.want {
				t.Errorf("dolt_maintenance_lock_key(%q, %q) = %q, want %q", tc.host, tc.port, got, tc.want)
			}
		})
	}
}

func TestDoltMaintenanceLockKeySameForCompactAndSyncSameEndpoint(t *testing.T) {
	// The whole point of sharing this helper is that compact and sync
	// derive the identical lock key for the same host:port so they
	// contend on one lock file instead of two independent ones.
	a := runRuntimeFunc(t, nil, "dolt_maintenance_lock_key", "127.0.0.1", "3307")
	b := runRuntimeFunc(t, nil, "dolt_maintenance_lock_key", "localhost", "3307")
	if a != b {
		t.Errorf("lock keys diverged for equivalent loopback hosts: %q vs %q", a, b)
	}
}

// writeFakeDoltForKill installs a fake `dolt` binary that answers the
// processlist scan with the given ids (one per line, CSV header "id" first)
// and logs every invocation (joined argv) to logPath.
func writeFakeDoltForKill(t *testing.T, dir string, ids []string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	csv := "id\n"
	for _, id := range ids {
		csv += id + "\n"
	}
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + shQuote(logPath) + "\n" +
		"case \"$*\" in\n" +
		"  *\"SELECT id FROM information_schema.processlist\"*)\n" +
		"    printf '" + strings.ReplaceAll(csv, "\n", `\n`) + "' ; exit 0 ;;\n" +
		"  *\"KILL QUERY\"*) exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	return logPath
}

func TestDoltKillStaleQueriesKillsMatchingIDs(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeFakeDoltForKill(t, binDir, []string{"42", "43"})

	out := runRuntimeFunc(t,
		[]string{"PATH=" + binDir + ":" + os.Getenv("PATH")},
		"dolt_kill_stale_queries", "127.0.0.1", "3307", "root", "CALL DOLT_FETCH('origin')")

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake dolt log: %v", err)
	}
	log := string(logBytes)

	if !strings.Contains(log, "SELECT id FROM information_schema.processlist WHERE info = 'CALL DOLT_FETCH(''origin'')'") {
		t.Errorf("processlist scan did not use exact-match escaped query; log:\n%s", log)
	}
	if !strings.Contains(log, "KILL QUERY 42") {
		t.Errorf("expected KILL QUERY 42 in log:\n%s", log)
	}
	if !strings.Contains(log, "KILL QUERY 43") {
		t.Errorf("expected KILL QUERY 43 in log:\n%s", log)
	}
	if !strings.Contains(out, "killing orphaned server-side query id=42") {
		t.Errorf("expected id=42 reclaim message in output:\n%s", out)
	}
}

func TestDoltKillStaleQueriesNoMatchesIsNoop(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeFakeDoltForKill(t, binDir, nil)

	runRuntimeFunc(t,
		[]string{"PATH=" + binDir + ":" + os.Getenv("PATH")},
		"dolt_kill_stale_queries", "127.0.0.1", "3307", "root", "CALL DOLT_GC()")

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake dolt log: %v", err)
	}
	if strings.Contains(string(logBytes), "KILL QUERY") {
		t.Errorf("expected no KILL QUERY calls when processlist scan returns no rows; log:\n%s", string(logBytes))
	}
}

func TestDoltKillStaleQueriesScanFailureIsNonFatal(t *testing.T) {
	binDir := t.TempDir()
	// A fake dolt that always fails simulates an unreachable / wedged
	// server during the reclaim scan itself; the helper must not error
	// out the calling script.
	body := "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}

	out := runRuntimeFunc(t,
		[]string{"PATH=" + binDir + ":" + os.Getenv("PATH")},
		"dolt_kill_stale_queries", "127.0.0.1", "3307", "root", "CALL DOLT_PUSH('origin')")

	if !strings.Contains(out, "could not scan processlist") {
		t.Errorf("expected a WARN about the failed scan, got:\n%s", out)
	}
}
