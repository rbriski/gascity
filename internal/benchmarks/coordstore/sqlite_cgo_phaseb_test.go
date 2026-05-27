//go:build cgo && sqlite_cgo

package coordstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/benchmarks/coordstore"
	sqlitecgo "github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/sqlite-cgo"
)

func TestMain(m *testing.M) {
	if backend := os.Getenv("CHAOS_SERVER_BACKEND"); backend != "" {
		os.Exit(runSQLiteCGOPhaseBChild(backend))
	}
	os.Exit(m.Run())
}

func TestSQLiteCGOPhaseBProductionDurability(t *testing.T) {
	if os.Getenv("COORDSTORE_SQLITE_PHASEB") == "" {
		t.Skip("set COORDSTORE_SQLITE_PHASEB=1 to run the SQLite-CGo Phase B durability gate")
	}

	ctx := context.Background()
	kills := envInt(t, "COORDSTORE_PHASEB_KILLS", 30)
	cadence := envDuration(t, "COORDSTORE_KILL_CADENCE", 30*time.Second)
	resultsDir := os.Getenv("COORDSTORE_RESULTS_DIR")
	if resultsDir == "" {
		resultsDir = filepath.Join(os.TempDir(), "coordstore-sqlite-cgo-phaseb-"+time.Now().UTC().Format("20060102T150405Z"))
	}
	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		t.Fatalf("mkdir results dir: %v", err)
	}

	dataDir := filepath.Join(t.TempDir(), "store")
	process := coordstore.NewChaosProcess(coordstore.ChaosProcessConfig{
		Backend:    "sqlite-cgo",
		SocketPath: filepath.Join(t.TempDir(), "chaos.sock"),
		DataDir:    dataDir,
	})
	if err := process.Start(ctx); err != nil {
		t.Fatalf("start chaos process: %v", err)
	}
	defer process.Close() //nolint:errcheck

	if err := process.Reset(ctx); err != nil {
		t.Fatalf("reset store: %v", err)
	}
	workload := coordstore.RealWorldWorkload
	workload.Duration = cadence*time.Duration(kills) + cadence/2
	seed, err := coordstore.NewSeeder(0x1234abcd).Seed(ctx, process, workload)
	if err != nil {
		t.Fatalf("seed workload: %v", err)
	}
	ackedWritesPath := filepath.Join(resultsDir, "acked-writes.jsonl")
	if err := process.ResetAckLedger(ackedWritesPath); err != nil {
		t.Fatalf("reset ack ledger: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	scoreCh := make(chan error, 1)
	go func() {
		_, runErr := coordstore.NewRunner(process, workload, seed).Run(runCtx, testWriter{t})
		scoreCh <- runErr
	}()

	events := make([]phaseBKillEvent, 0, kills)
	for i := 0; i < kills; i++ {
		select {
		case err := <-scoreCh:
			t.Fatalf("workload ended before kill %d/%d: %v", i+1, kills, err)
		case <-time.After(cadence):
		}
		event, err := runPhaseBKill(ctx, process)
		if err != nil {
			cancel()
			t.Fatalf("kill %d/%d: %v", i+1, kills, err)
		}
		events = append(events, event)
		if err := appendPhaseBEvent(filepath.Join(resultsDir, "kill-events.jsonl"), event); err != nil {
			cancel()
			t.Fatalf("write kill event: %v", err)
		}
		t.Logf("kill %d/%d: acked=%d missing=%d recovery=%s", i+1, kills, event.AckedIDs, event.MissingIDs, event.Recovery)
	}
	cancel()
	<-scoreCh

	report := summarizePhaseB(events)
	report.ResultsDir = resultsDir
	report.AckedWritesPath = ackedWritesPath
	report.KillEventsPath = filepath.Join(resultsDir, "kill-events.jsonl")
	if err := writePhaseBReport(filepath.Join(resultsDir, "phase-b-summary.json"), report); err != nil {
		t.Fatalf("write phase-b summary: %v", err)
	}
	t.Logf("phase-b artifacts: %s", resultsDir)
	t.Logf("phase-b summary: kills=%d acked=%d missing=%d loss_rate=%.6f", report.KillEvents, report.AckedWrites, report.MissingIDs, report.LossRate)
	if report.LossRate > 0.001 {
		t.Fatalf("SQLite-CGo Phase B loss rate %.6f exceeds 0.1%%", report.LossRate)
	}
}

type phaseBKillEvent struct {
	KilledAt        time.Time     `json:"killed_at"`
	LastAckAt       time.Time     `json:"last_ack_at,omitempty"`
	LossWindow      time.Duration `json:"loss_window"`
	LossWindowNanos int64         `json:"loss_window_nanos"`
	Recovery        time.Duration `json:"recovery"`
	RecoveryNanos   int64         `json:"recovery_nanos"`
	AckedIDs        int           `json:"acked_ids"`
	MissingIDs      int           `json:"missing_ids"`
	MissingIDSample []string      `json:"missing_id_sample,omitempty"`
}

type phaseBReport struct {
	ResultsDir       string  `json:"results_dir"`
	KillEventsPath   string  `json:"kill_events_path"`
	AckedWritesPath  string  `json:"acked_writes_path"`
	KillEvents       int     `json:"kill_events"`
	AckedWrites      int     `json:"acked_writes"`
	MissingIDs       int     `json:"missing_ids"`
	LossRate         float64 `json:"loss_rate"`
	MaxRecoveryNanos int64   `json:"max_recovery_nanos"`
}

func runPhaseBKill(ctx context.Context, process *coordstore.ChaosProcess) (phaseBKillEvent, error) {
	lastAck := process.LastAckTime()
	killedAt := time.Now().UTC()
	if err := process.Kill(ctx); err != nil {
		return phaseBKillEvent{}, err
	}
	recoveryStart := time.Now()
	restartDur, err := process.Restart(ctx)
	if err != nil {
		return phaseBKillEvent{}, err
	}
	if _, err := process.PrimeScan(ctx); err != nil {
		return phaseBKillEvent{}, err
	}
	recovery := time.Since(recoveryStart)
	if restartDur > recovery {
		recovery = restartDur
	}
	ackedIDs := process.AckedIDs()
	records, err := batchGetPhaseB(ctx, process, ackedIDs)
	if err != nil {
		return phaseBKillEvent{}, err
	}
	found := make(map[string]struct{}, len(records))
	for _, record := range records {
		found[record.ID] = struct{}{}
	}
	var missing []string
	for _, id := range ackedIDs {
		if _, ok := found[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 25 {
		missing = missing[:25]
	}
	lossWindow := time.Duration(0)
	if !lastAck.IsZero() {
		lossWindow = killedAt.Sub(lastAck)
	}
	return phaseBKillEvent{
		KilledAt:        killedAt,
		LastAckAt:       lastAck,
		LossWindow:      lossWindow,
		LossWindowNanos: lossWindow.Nanoseconds(),
		Recovery:        recovery,
		RecoveryNanos:   recovery.Nanoseconds(),
		AckedIDs:        len(ackedIDs),
		MissingIDs:      len(ackedIDs) - len(records),
		MissingIDSample: missing,
	}, nil
}

func batchGetPhaseB(ctx context.Context, adapter coordstore.StoreAdapter, ids []string) ([]coordstore.Record, error) {
	records := make([]coordstore.Record, 0, len(ids))
	for start := 0; start < len(ids); start += coordstore.TerminalPurgeBatchSize {
		end := start + coordstore.TerminalPurgeBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch, err := adapter.BatchGet(ctx, ids[start:end])
		if err != nil {
			return nil, fmt.Errorf("batch get acked ids[%d:%d]: %w", start, end, err)
		}
		records = append(records, batch...)
	}
	return records, nil
}

func appendPhaseBEvent(path string, event phaseBKillEvent) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close() //nolint:errcheck
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	return file.Sync()
}

func summarizePhaseB(events []phaseBKillEvent) phaseBReport {
	var report phaseBReport
	report.KillEvents = len(events)
	for _, event := range events {
		report.AckedWrites += event.AckedIDs
		report.MissingIDs += event.MissingIDs
		if event.RecoveryNanos > report.MaxRecoveryNanos {
			report.MaxRecoveryNanos = event.RecoveryNanos
		}
	}
	if report.AckedWrites > 0 {
		report.LossRate = float64(report.MissingIDs) / float64(report.AckedWrites)
	}
	return report
}

func writePhaseBReport(path string, report phaseBReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func runSQLiteCGOPhaseBChild(backend string) int {
	if backend != "sqlite-cgo" {
		fmt.Fprintf(os.Stderr, "unknown phase-b backend %q\n", backend)
		return 2
	}
	socketPath := os.Getenv("CHAOS_SERVER_SOCKET")
	dataDir := os.Getenv("CHAOS_SERVER_DATA_DIR")
	if socketPath == "" || dataDir == "" {
		fmt.Fprintf(os.Stderr, "CHAOS_SERVER_SOCKET and CHAOS_SERVER_DATA_DIR are required\n")
		return 2
	}
	adapter := sqlitecgo.New()
	if err := adapter.Open(context.Background(), coordstore.Config{DataDir: dataDir}); err != nil {
		fmt.Fprintf(os.Stderr, "open sqlite-cgo chaos adapter: %v\n", err)
		return 2
	}
	defer adapter.Close() //nolint:errcheck
	if err := coordstore.RunChaosServer(context.Background(), adapter, socketPath, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "run sqlite-cgo chaos server: %v\n", err)
		return 2
	}
	return 0
}

func envInt(t *testing.T, key string, fallback int) int {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		t.Fatalf("invalid %s=%q", key, raw)
	}
	return value
}

func envDuration(t *testing.T, key string, fallback time.Duration) time.Duration {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		t.Fatalf("invalid %s=%q", key, raw)
	}
	return value
}
