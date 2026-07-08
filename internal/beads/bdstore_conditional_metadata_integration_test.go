//go:build integration

package beads_test

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestBdStoreSetMetadataIfRealBd pins the BdStore conditional-metadata CAS
// against a real bd + Dolt SQL server: the raw `bd sql` UPDATE with JSON_SET /
// JSON_UNQUOTE / COALESCE must behave as the fake-runner unit tests assert —
// rows_affected drives the swap, the dotted key resolves as one JSON member, and
// the compare reads the metadata JSON string value. Skips cleanly when bd, dolt,
// or git are unavailable, or when any bootstrap step fails (the raw-SQL path
// bypasses bd hooks/daemon cache by design, exactly as ReleaseIfCurrent does).
func TestBdStoreSetMetadataIfRealBd(t *testing.T) {
	bdBin, err := exec.LookPath("bd")
	if err != nil {
		t.Skip("bd not on PATH; skipping real-bd conditional-metadata pin")
	}
	doltBin, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not on PATH; skipping real-bd conditional-metadata pin")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping real-bd conditional-metadata pin")
	}

	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	envMap := map[string]string{
		"HOME":                home,
		"GIT_AUTHOR_NAME":     "cas-test",
		"GIT_AUTHOR_EMAIL":    "cas@example.com",
		"GIT_COMMITTER_NAME":  "cas-test",
		"GIT_COMMITTER_EMAIL": "cas@example.com",
	}
	env := append(os.Environ(),
		"HOME="+home,
		"GIT_AUTHOR_NAME=cas-test", "GIT_AUTHOR_EMAIL=cas@example.com",
		"GIT_COMMITTER_NAME=cas-test", "GIT_COMMITTER_EMAIL=cas@example.com",
	)

	// Isolated git workspace (bd init requires one).
	ws := filepath.Join(root, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatalf("mkdir ws: %v", err)
	}
	runOrSkip(t, env, ws, "git", "init", "--quiet")

	// Shared Dolt SQL server on a free port.
	port := freePort(t)
	doltData := filepath.Join(root, "dolt")
	if err := os.MkdirAll(doltData, 0o755); err != nil {
		t.Fatalf("mkdir dolt data: %v", err)
	}
	serverCtx, cancelServer := context.WithCancel(context.Background())
	server := exec.CommandContext(serverCtx, doltBin, "sql-server", "-H", "127.0.0.1", "-P", port, "--data-dir", doltData)
	server.Env = env
	if err := server.Start(); err != nil {
		cancelServer()
		t.Skipf("dolt sql-server failed to start: %v", err)
	}
	serverDone := make(chan struct{})
	go func() { _ = server.Wait(); close(serverDone) }()
	// One cleanup that cancels FIRST, then waits (bounded): t.Cleanup runs LIFO,
	// so a separate wait-cleanup would block before the cancel ever ran.
	t.Cleanup(func() {
		cancelServer()
		if server.Process != nil {
			_ = server.Process.Kill()
		}
		select {
		case <-serverDone:
		case <-time.After(10 * time.Second):
		}
	})
	if !waitForPort(net.JoinHostPort("127.0.0.1", port), 10*time.Second) {
		t.Skip("dolt sql-server did not become ready; skipping")
	}

	// Initialize beads against the shared server.
	initCtx, cancelInit := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelInit()
	bdInit := exec.CommandContext(initCtx, bdBin, "init", "--server",
		"--server-host", "127.0.0.1", "--server-port", port,
		"-p", "cas", "--skip-hooks", "--skip-agents")
	bdInit.Dir = ws
	bdInit.Env = env
	if out, err := bdInit.CombinedOutput(); err != nil {
		t.Skipf("bd init failed (%v): %s", err, out)
	}

	store := beads.NewBdStore(ws, beads.ExecCommandRunnerWithEnv(envMap))
	ctx := context.Background()
	const key = "gc.control_epoch"

	created, err := store.Create(beads.Bead{Title: "real-bd cas", Metadata: map[string]string{key: "1"}})
	if err != nil {
		if errors.Is(err, beads.ErrBDSilentFallback) {
			t.Skipf("bd tripped the silent-fallback guard on Create: %v", err)
		}
		t.Fatalf("Create: %v", err)
	}
	id := created.ID

	valueOf := func() string {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		return got.Metadata[key]
	}
	if v := valueOf(); v != "1" {
		t.Fatalf("seed value = %q, want 1 (dotted key must round-trip as one JSON member)", v)
	}

	// Match → swaps, value advances.
	swapped, err := store.SetMetadataIf(ctx, id, key, "1", "2")
	if err != nil {
		t.Fatalf("SetMetadataIf (match): %v", err)
	}
	if !swapped {
		t.Fatal("swapped = false, want true on a matching precondition against real bd")
	}
	if v := valueOf(); v != "2" {
		t.Fatalf("value after match = %q, want 2", v)
	}

	// Mismatch → no-op, value unchanged, no error (stale expected).
	swapped, err = store.SetMetadataIf(ctx, id, key, "1", "3")
	if err != nil {
		t.Fatalf("SetMetadataIf (mismatch) errored, want (false, nil): %v", err)
	}
	if swapped {
		t.Fatal("swapped = true, want false when the observed value moved out from under expected")
	}
	if v := valueOf(); v != "2" {
		t.Fatalf("value after mismatch = %q, want unchanged 2", v)
	}

	// Set-to-same when the precondition holds → swaps (Go read path, no UPDATE).
	swapped, err = store.SetMetadataIf(ctx, id, key, "2", "2")
	if err != nil {
		t.Fatalf("SetMetadataIf (set-to-same): %v", err)
	}
	if !swapped {
		t.Fatal("swapped = false, want true for a precondition-holding no-op")
	}
	if v := valueOf(); v != "2" {
		t.Fatalf("value after set-to-same = %q, want 2", v)
	}

	// Absent-key first write: expected == "" must match an ABSENT key through the
	// COALESCE(JSON_UNQUOTE(JSON_EXTRACT(...)), '') NULL-fold (P5.2's first-write /
	// unset-epoch case) and swap it to the first value.
	const freshKey = "gc.fresh_epoch"
	swapped, err = store.SetMetadataIf(ctx, id, freshKey, "", "first")
	if err != nil {
		t.Fatalf("SetMetadataIf (absent-key first write): %v", err)
	}
	if !swapped {
		t.Fatal("swapped = false, want true: expected=\"\" must match an absent key via the COALESCE NULL-fold")
	}
	if got, err := store.Get(id); err != nil {
		t.Fatalf("Get after absent-key write: %v", err)
	} else if got.Metadata[freshKey] != "first" {
		t.Fatalf("absent-key first write = %q, want first", got.Metadata[freshKey])
	}

	// next == "" clears the matching key observably via JSON_SET, honoring the
	// empty-string clear contract.
	swapped, err = store.SetMetadataIf(ctx, id, key, "2", "")
	if err != nil {
		t.Fatalf("SetMetadataIf (clear via next=\"\"): %v", err)
	}
	if !swapped {
		t.Fatal("swapped = false, want true when clearing a matching key to empty")
	}
	if v := valueOf(); v != "" {
		t.Fatalf("value after next=\"\" clear = %q, want observable empty", v)
	}

	// Dolt-backed concurrent single-winner: two conditional UPDATEs against the
	// same seed race through the shared Dolt server; exactly one lands a row
	// (rows_affected>0) and the final value is that winner's. This is the real
	// serialization proof the in-memory fake cannot give — the losing UPDATE's
	// WHERE no longer matches once the winner commits.
	raced, err := store.Create(beads.Bead{Title: "real-bd cas race", Metadata: map[string]string{key: "0"}})
	if err != nil {
		if errors.Is(err, beads.ErrBDSilentFallback) {
			t.Skipf("bd tripped the silent-fallback guard creating race bead: %v", err)
		}
		t.Fatalf("Create race bead: %v", err)
	}
	nexts := []string{"A", "B"}
	type raceOutcome struct {
		next    string
		swapped bool
		err     error
	}
	outcomes := make([]raceOutcome, len(nexts))
	var wg sync.WaitGroup
	startRace := make(chan struct{})
	for i, next := range nexts {
		wg.Add(1)
		go func(i int, next string) {
			defer wg.Done()
			<-startRace
			sw, e := store.SetMetadataIf(ctx, raced.ID, key, "0", next)
			outcomes[i] = raceOutcome{next: next, swapped: sw, err: e}
		}(i, next)
	}
	close(startRace)
	wg.Wait()

	winners := 0
	winningNext := ""
	for _, o := range outcomes {
		if o.err != nil {
			t.Fatalf("concurrent SetMetadataIf(next=%q) errored, want at most one winner and never an error: %v", o.next, o.err)
		}
		if o.swapped {
			winners++
			winningNext = o.next
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1 (single-winner conditional UPDATE)", winners)
	}
	if got, err := store.Get(raced.ID); err != nil {
		t.Fatalf("Get race bead: %v", err)
	} else if got.Metadata[key] != winningNext {
		t.Fatalf("final race value = %q, want the winner's %q", got.Metadata[key], winningNext)
	}

	// Follow-up (deferred): a real NativeDoltStore concurrency pin against this
	// shared server needs a separate native-store open (OpenNativeDoltStoreAt with
	// scoped BEADS_DOLT_* env and its own .beads scope). Its read-compare-write
	// rides Dolt's transaction retry (withRetry) rather than a single conditional
	// UPDATE, and the deterministic withRetry-replay hazard that path carries is
	// already pinned by TestNativeDoltStoreSetMetadataIfReplayDoesNotPhantomWin —
	// a nondeterministic race would only exercise it occasionally.
}

func runOrSkip(t *testing.T, env []string, dir, name string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("%s %v failed (%v): %s", name, args, err, out)
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("allocating port: %v", err)
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	_ = l.Close()
	return port
}

func waitForPort(addr string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
