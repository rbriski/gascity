// Package dolt_test validates that the compactor.sh exec order script
// replaces the obsolete mol-dog-compactor formula. The test exercises
// the script against a stubbed dolt binary that logs its arguments,
// asserting that the script:
//
//  1. Passes the projected Dolt connection target (--host/--port/--user/
//     --no-tls) and never leaks the password.
//  2. Skips databases below the commit threshold without issuing a
//     flatten recipe.
//  3. Executes the full flatten recipe (branch + soft reset + commit +
//     swap + gc) against databases above the threshold.
//  4. Honours the dry-run toggle — no mutating SQL is emitted.
package dolt_test

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const compactorScript = "assets/scripts/compactor.sh"

// compactorDoltStub is a fake dolt binary used by the compactor tests.
// It logs every invocation's argv to $DOLT_ARGS_LOG (so the test can
// assert on the emitted SQL shape) and returns canned responses for
// the queries the compactor issues. COMPACTOR_TEST_COMMITS lets a
// test swap the reported commit count to drive the threshold branch.
const compactorDoltStub = `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
# Pass-through for multi-statement stdin (flatten recipe) — the test
# records the stdin body so it can assert DOLT_RESET/DOLT_COMMIT shape.
if [ -n "$DOLT_STDIN_LOG" ]; then
    cat >> "$DOLT_STDIN_LOG" 2>/dev/null || true
fi
case "$*" in
    *"SHOW DATABASES"*)
        printf 'Database\ntest_db\n'
        ;;
    *"information_schema.tables"*)
        printf 'table_name\nissues\n'
        ;;
    *"FROM dolt_log"*)
        # Default to the "above threshold" count; tests that want the
        # skip-path override via COMPACTOR_TEST_COMMITS.
        printf 'COUNT(*)\n%s\n' "${COMPACTOR_TEST_COMMITS:-9999}"
        ;;
    *issues*)
        printf 'COUNT(*)\n42\n'
        ;;
    *)
        :
        ;;
esac
exit 0
`

func writeCompactorDoltStub(t *testing.T, path string) {
	t.Helper()
	writeExecutable(t, path, compactorDoltStub)
}

func writeCompactorGCStub(t *testing.T, path, logPath string) {
	t.Helper()
	writeExecutable(t, path, `#!/bin/sh
printf '%s\n' "$*" >> "`+logPath+`"
exit 0
`)
}

func compactorScriptPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), compactorScript)
}

func runCompactor(t *testing.T, env map[string]string) (stdout string) {
	t.Helper()
	script := compactorScriptPath(t)
	cmd := exec.Command(script)
	base := filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_PORT", "GC_DOLT_HOST",
		"GC_DOLT_USER", "GC_DOLT_PASSWORD", "GC_COMPACTOR_THRESHOLD",
		"GC_COMPACTOR_DRY_RUN", "DOLT_ARGS_LOG", "DOLT_STDIN_LOG",
		"COMPACTOR_TEST_COMMITS", "PATH")
	for k, v := range env {
		base = append(base, k+"="+v)
	}
	cmd.Env = base
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("compactor.sh failed: %v\n%s", err, out)
	}
	return string(out)
}

func TestCompactorScriptUsesProjectedConnectionTarget(t *testing.T) {
	cityPath := t.TempDir()
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	stdinLog := filepath.Join(t.TempDir(), "dolt-stdin.log")

	writeCompactorDoltStub(t, filepath.Join(binDir, "dolt"))
	writeCompactorGCStub(t, filepath.Join(binDir, "gc"), filepath.Join(t.TempDir(), "gc.log"))

	runCompactor(t, map[string]string{
		"GC_CITY_PATH":     cityPath,
		"GC_PACK_DIR":      repoRoot(t),
		"GC_DOLT_HOST":     "external.example.internal",
		"GC_DOLT_PORT":     "4406",
		"GC_DOLT_USER":     "compactor-user",
		"GC_DOLT_PASSWORD": "secret-password",
		"DOLT_ARGS_LOG":    doltLog,
		"DOLT_STDIN_LOG":   stdinLog,
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})

	logData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	log := string(logData)
	for _, want := range []string{
		"--host external.example.internal",
		"--port 4406",
		"--user compactor-user",
		"--no-tls",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("dolt calls missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "secret-password") {
		t.Fatalf("dolt password leaked into argv log:\n%s", log)
	}
}

func TestCompactorScriptFallsBackToManagedRuntimePort(t *testing.T) {
	cityPath := t.TempDir()
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	stdinLog := filepath.Join(t.TempDir(), "dolt-stdin.log")

	listener := listenManagedDoltPortT(t)
	port := listener.Addr().(*net.TCPAddr).Port
	writeManagedRuntimeStateForScript(t, cityPath, port)

	writeCompactorDoltStub(t, filepath.Join(binDir, "dolt"))
	writeCompactorGCStub(t, filepath.Join(binDir, "gc"), filepath.Join(t.TempDir(), "gc.log"))

	runCompactor(t, map[string]string{
		"GC_CITY_PATH":   cityPath,
		"GC_PACK_DIR":    repoRoot(t),
		"DOLT_ARGS_LOG":  doltLog,
		"DOLT_STDIN_LOG": stdinLog,
		"PATH":           binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})

	logData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	if want := "--port " + strconv.Itoa(port); !strings.Contains(string(logData), want) {
		t.Fatalf("dolt calls missing %q:\n%s", want, logData)
	}
}

func TestCompactorScriptSkipsBelowThreshold(t *testing.T) {
	cityPath := t.TempDir()
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	stdinLog := filepath.Join(t.TempDir(), "dolt-stdin.log")

	writeCompactorDoltStub(t, filepath.Join(binDir, "dolt"))
	writeCompactorGCStub(t, filepath.Join(binDir, "gc"), filepath.Join(t.TempDir(), "gc.log"))

	runCompactor(t, map[string]string{
		"GC_CITY_PATH":           cityPath,
		"GC_PACK_DIR":            repoRoot(t),
		"GC_DOLT_HOST":           "127.0.0.1",
		"GC_DOLT_PORT":           "3307",
		"GC_DOLT_USER":           "root",
		"GC_DOLT_PASSWORD":       "",
		"GC_COMPACTOR_THRESHOLD": "500",
		"COMPACTOR_TEST_COMMITS": "10",
		"DOLT_ARGS_LOG":          doltLog,
		"DOLT_STDIN_LOG":         stdinLog,
		"PATH":                   binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})

	logData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	for _, unwanted := range []string{"DOLT_RESET", "DOLT_COMMIT", "DOLT_GC"} {
		if strings.Contains(string(logData), unwanted) {
			t.Fatalf("below-threshold run issued %s:\n%s", unwanted, logData)
		}
	}
	stdinData, _ := os.ReadFile(stdinLog)
	for _, unwanted := range []string{"DOLT_RESET", "DOLT_COMMIT", "DOLT_GC"} {
		if strings.Contains(string(stdinData), unwanted) {
			t.Fatalf("below-threshold run issued %s via stdin:\n%s", unwanted, stdinData)
		}
	}
}

func TestCompactorScriptRunsFlattenAboveThreshold(t *testing.T) {
	cityPath := t.TempDir()
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	stdinLog := filepath.Join(t.TempDir(), "dolt-stdin.log")

	writeCompactorDoltStub(t, filepath.Join(binDir, "dolt"))
	writeCompactorGCStub(t, filepath.Join(binDir, "gc"), filepath.Join(t.TempDir(), "gc.log"))

	runCompactor(t, map[string]string{
		"GC_CITY_PATH":           cityPath,
		"GC_PACK_DIR":            repoRoot(t),
		"GC_DOLT_HOST":           "127.0.0.1",
		"GC_DOLT_PORT":           "3307",
		"GC_DOLT_USER":           "root",
		"GC_DOLT_PASSWORD":       "",
		"GC_COMPACTOR_THRESHOLD": "500",
		"COMPACTOR_TEST_COMMITS": "9999",
		"DOLT_ARGS_LOG":          doltLog,
		"DOLT_STDIN_LOG":         stdinLog,
		"PATH":                   binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})

	// The flatten recipe is issued via a multi-statement heredoc on
	// stdin; the single-statement argv log captures the GC call on a
	// separate session.
	stdinData, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt stdin log): %v", err)
	}
	stdin := string(stdinData)
	for _, want := range []string{
		"CALL DOLT_BRANCH('flatten-tmp')",
		"CALL DOLT_CHECKOUT('flatten-tmp')",
		"CALL DOLT_RESET('--soft'",
		"CALL DOLT_COMMIT('-Am'",
		"CALL DOLT_CHECKOUT('main')",
		"CALL DOLT_RESET('--hard', 'flatten-tmp')",
		"CALL DOLT_BRANCH('-D', 'flatten-tmp')",
	} {
		if !strings.Contains(stdin, want) {
			t.Fatalf("flatten recipe missing %q:\n%s", want, stdin)
		}
	}

	// DOLT_GC must run on its own session (outside any implicit txn)
	// — look for it in the argv log, not the multi-statement stdin.
	argLog, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt args log): %v", err)
	}
	if !strings.Contains(string(argLog), "DOLT_GC") {
		t.Fatalf("DOLT_GC not invoked via its own session:\n%s", argLog)
	}
}

func TestCompactorScriptDryRunSkipsMutation(t *testing.T) {
	cityPath := t.TempDir()
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	stdinLog := filepath.Join(t.TempDir(), "dolt-stdin.log")

	writeCompactorDoltStub(t, filepath.Join(binDir, "dolt"))
	writeCompactorGCStub(t, filepath.Join(binDir, "gc"), filepath.Join(t.TempDir(), "gc.log"))

	out := runCompactor(t, map[string]string{
		"GC_CITY_PATH":           cityPath,
		"GC_PACK_DIR":            repoRoot(t),
		"GC_DOLT_HOST":           "127.0.0.1",
		"GC_DOLT_PORT":           "3307",
		"GC_DOLT_USER":           "root",
		"GC_DOLT_PASSWORD":       "",
		"GC_COMPACTOR_THRESHOLD": "500",
		"GC_COMPACTOR_DRY_RUN":   "1",
		"COMPACTOR_TEST_COMMITS": "9999",
		"DOLT_ARGS_LOG":          doltLog,
		"DOLT_STDIN_LOG":         stdinLog,
		"PATH":                   binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})

	if !strings.Contains(out, "would flatten") {
		t.Fatalf("dry-run output missing 'would flatten':\n%s", out)
	}
	stdinData, _ := os.ReadFile(stdinLog)
	for _, unwanted := range []string{"DOLT_RESET", "DOLT_COMMIT"} {
		if strings.Contains(string(stdinData), unwanted) {
			t.Fatalf("dry-run issued %s:\n%s", unwanted, stdinData)
		}
	}
	argData, _ := os.ReadFile(doltLog)
	if strings.Contains(string(argData), "DOLT_GC") {
		t.Fatalf("dry-run invoked DOLT_GC:\n%s", argData)
	}
}

func TestCompactorOrderIsExecBased(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "orders", "mol-dog-compactor.toml"))
	if err != nil {
		t.Fatalf("ReadFile(order): %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "exec") {
		t.Fatalf("mol-dog-compactor.toml is not exec-based:\n%s", body)
	}
	for _, unwanted := range []string{"formula =", "pool ="} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("mol-dog-compactor.toml still carries %q:\n%s", unwanted, body)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "formulas", "mol-dog-compactor.toml")); !os.IsNotExist(err) {
		t.Fatalf("formulas/mol-dog-compactor.toml must be removed; stat err=%v", err)
	}
}

func listenManagedDoltPortT(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}
