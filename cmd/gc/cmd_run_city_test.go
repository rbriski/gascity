package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

func writeRunSourceAndIR(t *testing.T, cityPath string) string {
	t.Helper()
	sourcePath := filepath.Join(cityPath, "review.lumen")
	if err := os.WriteFile(sourcePath, []byte("formula source is compiled separately\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	raw, err := json.Marshal(tbHookDoc(t))
	if err != nil {
		t.Fatalf("marshal IR: %v", err)
	}
	if err := os.WriteFile(sourcePath+".json", raw, 0o644); err != nil {
		t.Fatalf("write sibling IR: %v", err)
	}
	return sourcePath
}

func TestRunInResolvedCityEnqueuesAndWaits(t *testing.T) {
	cityPath := tbHookGraphCity(t)
	sourcePath := writeRunSourceAndIR(t, cityPath)
	pokes := stubPokeLumenRuns(t)

	origResolver := resolveRunCity
	resolveRunCity = func() (string, error) { return cityPath, nil }
	t.Cleanup(func() { resolveRunCity = origResolver })

	origWaiter := runWaitForLumenRun
	var waitedStream string
	runWaitForLumenRun = func(_ context.Context, store *graphstore.Store, streamID string, _ time.Duration) (engine.RunResult, error) {
		if store == nil {
			t.Fatal("waiter received nil graph store")
		}
		waitedStream = streamID
		return engine.RunResult{StreamID: streamID, Outcome: engine.OutcomePass}, nil
	}
	t.Cleanup(func() { runWaitForLumenRun = origWaiter })

	var stdout, stderr bytes.Buffer
	cmd := newRunCmd(&stdout, &stderr)
	cmd.SetArgs([]string{sourcePath, "--route", tbHookRoute, "--input", `{"topic":"gears"}`})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc run failed: %v\nstderr: %s", err, stderr.String())
	}
	if waitedStream == "" {
		t.Fatal("gc run did not wait for the enqueued stream")
	}
	if *pokes != 1 {
		t.Fatalf("controller poke count = %d, want 1", *pokes)
	}

	gs := tbHookOpenStore(t, cityPath)
	defer func() { _ = gs.Close() }()
	manifest, err := engine.ReadRunManifest(context.Background(), gs, waitedStream)
	if err != nil {
		t.Fatalf("read enqueued manifest: %v", err)
	}
	if manifest.FormulaRef != sourcePath+".json" {
		t.Fatalf("formula ref = %q, want sibling IR %q", manifest.FormulaRef, sourcePath+".json")
	}
	if manifest.DefaultRoute != tbHookRoute {
		t.Fatalf("default route = %q, want %q", manifest.DefaultRoute, tbHookRoute)
	}
	if manifest.Driver != "" {
		t.Fatalf("driver = %q, want pool/controller driver", manifest.Driver)
	}

	out := stdout.String()
	streamAt := strings.Index(out, waitedStream)
	outcomeAt := strings.Index(out, "outcome: pass")
	if streamAt < 0 || outcomeAt < 0 || streamAt > outcomeAt {
		t.Fatalf("stdout must expose stream before terminal outcome:\n%s", out)
	}
}

func TestRunInResolvedCityPropagatesTerminalStatus(t *testing.T) {
	tests := []struct {
		name       string
		result     engine.RunResult
		waitErr    error
		wantCode   int
		wantOutput string
	}{
		{name: "failed formula", result: engine.RunResult{Outcome: engine.OutcomeFailed}, wantCode: 1, wantOutput: "outcome: failed"},
		{name: "canceled waiter detaches", waitErr: context.Canceled, wantCode: 130, wantOutput: "run continues in city"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := tbHookGraphCity(t)
			sourcePath := writeRunSourceAndIR(t, cityPath)
			stubPokeLumenRuns(t)

			origResolver := resolveRunCity
			resolveRunCity = func() (string, error) { return cityPath, nil }
			t.Cleanup(func() { resolveRunCity = origResolver })
			origWaiter := runWaitForLumenRun
			runWaitForLumenRun = func(_ context.Context, _ *graphstore.Store, streamID string, _ time.Duration) (engine.RunResult, error) {
				result := tt.result
				result.StreamID = streamID
				return result, tt.waitErr
			}
			t.Cleanup(func() { runWaitForLumenRun = origWaiter })

			var stdout, stderr bytes.Buffer
			cmd := newRunCmd(&stdout, &stderr)
			cmd.SetArgs([]string{sourcePath, "--route", tbHookRoute})
			err := cmd.Execute()
			if got := commandExitCode(err); got != tt.wantCode {
				t.Fatalf("exit code = %d (err %v), want %d; stderr: %s", got, err, tt.wantCode, stderr.String())
			}
			combined := stdout.String() + stderr.String()
			if !strings.Contains(combined, tt.wantOutput) {
				t.Fatalf("output missing %q:\n%s", tt.wantOutput, combined)
			}
		})
	}
}

func TestRunCityResolutionErrorDoesNotFallBackToStandalone(t *testing.T) {
	cityPath := tbHookGraphCity(t)
	sourcePath := writeRunSourceAndIR(t, cityPath)

	origResolver := resolveRunCity
	resolveRunCity = func() (string, error) {
		return "", errors.New("explicit city selector is not registered")
	}
	t.Cleanup(func() { resolveRunCity = origResolver })

	var stdout, stderr bytes.Buffer
	cmd := newRunCmd(&stdout, &stderr)
	cmd.SetArgs([]string{sourcePath, "--route", tbHookRoute})
	err := cmd.Execute()
	if got := commandExitCode(err); got != 1 {
		t.Fatalf("exit code = %d (err %v), want 1", got, err)
	}
	if !strings.Contains(stderr.String(), "explicit city selector is not registered") {
		t.Fatalf("stderr = %q, want the City resolution error", stderr.String())
	}
	if strings.Contains(stderr.String(), "pass --agent-cmd") {
		t.Fatalf("stderr = %q, must not enter the standalone runner", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want no standalone run output", stdout.String())
	}
}

func TestImplicitRunCityMissClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "typed miss", err: errImplicitCityNotFound, want: true},
		{name: "wrapped typed miss", err: fmt.Errorf("discovering City: %w", errImplicitCityNotFound), want: true},
		{name: "same text is not the typed miss", err: errors.New(errImplicitCityNotFound.Error()), want: false},
		{name: "explicit resolution error", err: errors.New("explicit city selector is not registered"), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isImplicitRunCityMiss(tt.err); got != tt.want {
				t.Fatalf("isImplicitRunCityMiss(%v) = %t, want %t", tt.err, got, tt.want)
			}
		})
	}
}

func TestWaitForLumenRunObservesSealAndCancellationDoesNotClose(t *testing.T) {
	t.Run("sealed", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		gs := tbHookOpenStore(t, cityPath)
		defer func() { _ = gs.Close() }()

		completed, err := engine.Run(context.Background(), gs, lumenExecDoc(t, "completed"), map[string]any{})
		if err != nil {
			t.Fatalf("seed completed run: %v", err)
		}
		result, err := waitForLumenRun(context.Background(), gs, completed.StreamID, time.Millisecond)
		if err != nil {
			t.Fatalf("wait for completed run: %v", err)
		}
		if result.Outcome != engine.OutcomePass {
			t.Fatalf("outcome = %q, want pass", result.Outcome)
		}
		var closes int
		for _, event := range result.Events {
			if event.Type == engine.EventRunClosed {
				closes++
			}
		}
		if closes != 1 {
			t.Fatalf("run.closed count = %d, want 1", closes)
		}
	})

	t.Run("canceled waiter leaves run open", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		gs := tbHookOpenStore(t, cityPath)
		defer func() { _ = gs.Close() }()

		streamID, err := engine.EnqueueRun(context.Background(), gs, tbHookDoc(t), map[string]any{}, "review.lumen.json", tbHookRoute)
		if err != nil {
			t.Fatalf("seed open run: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = waitForLumenRun(ctx, gs, streamID, time.Hour)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wait error = %v, want context.Canceled", err)
		}
		runs, err := engine.ListOpenRuns(context.Background(), gs)
		if err != nil {
			t.Fatalf("list open runs: %v", err)
		}
		if len(runs) != 1 || runs[0].StreamID != streamID {
			t.Fatalf("open runs = %+v, want durable run %q still open", runs, streamID)
		}
	})
}

func TestResolveLumenIRPath(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "review.lumen")
	direct := source + ".json"
	if err := os.WriteFile(source, []byte("source"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := os.WriteFile(direct, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write IR: %v", err)
	}

	if got, err := resolveLumenIRPath(source); err != nil || got != direct {
		t.Fatalf("resolve source = %q, %v; want %q", got, err, direct)
	}
	if got, err := resolveLumenIRPath(direct); err != nil || got != direct {
		t.Fatalf("resolve direct IR = %q, %v; want %q", got, err, direct)
	}
	missing := filepath.Join(dir, "missing.lumen")
	if _, err := resolveLumenIRPath(missing); err == nil || !strings.Contains(err.Error(), "no compiled IR found") {
		t.Fatalf("missing sibling error = %v, want no compiled IR found", err)
	}
}
