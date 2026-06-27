package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeHealthCache writes a minimal provider-health.json to dir.
func writeHealthCache(t *testing.T, dir, provider, status string, probedAt float64) {
	t.Helper()
	cacheDir := filepath.Join(dir, ".gc", "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdirAll: %v", err)
	}
	rec := map[string]any{"provider": provider, "status": status, "probed_at": probedAt}
	data, _ := json.Marshal(map[string]any{"providers": []any{rec}})
	if err := os.WriteFile(filepath.Join(cacheDir, "provider-health.json"), data, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}
}

func nowSecs() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// --- providerHealthSnapshot tests ---

func TestSnapshotCheck_HealthyProvider(t *testing.T) {
	dir := t.TempDir()
	writeHealthCache(t, dir, "zai", "healthy", nowSecs())
	snap := loadProviderHealthSnapshot(dir)
	status, present := snap.check("zai")
	if !present {
		t.Fatal("expected registryPresent=true")
	}
	if status != providerStatusGreen {
		t.Fatalf("expected green, got %v", status)
	}
}

func TestSnapshotCheck_UnhealthyProvider(t *testing.T) {
	dir := t.TempDir()
	writeHealthCache(t, dir, "zai", "unhealthy", nowSecs())
	snap := loadProviderHealthSnapshot(dir)
	status, present := snap.check("zai")
	if !present {
		t.Fatal("expected registryPresent=true")
	}
	if status != providerStatusRed {
		t.Fatalf("expected red, got %v", status)
	}
}

func TestSnapshotCheck_RegistryAbsent(t *testing.T) {
	dir := t.TempDir() // no file
	snap := loadProviderHealthSnapshot(dir)
	status, present := snap.check("zai")
	if present {
		t.Fatal("expected registryPresent=false when file absent")
	}
	if status != providerStatusGreen {
		// fail-open: missing registry → treat as green
		t.Fatal("expected green (fail-open) when registry absent")
	}
}

func TestSnapshotCheck_StaleEntry(t *testing.T) {
	dir := t.TempDir()
	staleAt := nowSecs() - (providerHealthTTL.Seconds() + 10)
	writeHealthCache(t, dir, "zai", "unhealthy", staleAt)
	snap := loadProviderHealthSnapshot(dir)
	status, present := snap.check("zai")
	if present {
		t.Fatal("expected registryPresent=false for stale entry")
	}
	if status != providerStatusGreen {
		t.Fatal("expected green (fail-open) for stale entry")
	}
}

func TestSnapshotCheck_UnknownProvider(t *testing.T) {
	dir := t.TempDir()
	writeHealthCache(t, dir, "anthropic", "healthy", nowSecs())
	snap := loadProviderHealthSnapshot(dir)
	status, present := snap.check("zai") // different provider
	if present {
		t.Fatal("expected registryPresent=false for unknown provider")
	}
	if status != providerStatusGreen {
		t.Fatal("expected green (fail-open) for unknown provider")
	}
}

func TestSnapshotHealthyProviders(t *testing.T) {
	dir := t.TempDir()
	// Write two providers: one healthy, one not.
	cacheDir := filepath.Join(dir, ".gc", "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdirAll: %v", err)
	}
	data, _ := json.Marshal(map[string]any{"providers": []any{
		map[string]any{"provider": "zai", "status": "healthy", "probed_at": nowSecs()},
		map[string]any{"provider": "anthropic", "status": "unhealthy", "probed_at": nowSecs()},
	}})
	if err := os.WriteFile(filepath.Join(cacheDir, "provider-health.json"), data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	snap := loadProviderHealthSnapshot(dir)
	healthy := snap.healthyProviders()
	if len(healthy) != 1 || healthy[0] != "zai" {
		t.Fatalf("expected [zai], got %v", healthy)
	}
}

func TestSnapshotCheck_ThrottledProvider(t *testing.T) {
	dir := t.TempDir()
	writeHealthCache(t, dir, "zai", "throttled", nowSecs())
	snap := loadProviderHealthSnapshot(dir)
	status, present := snap.check("zai")
	if !present {
		t.Fatal("expected registryPresent=true")
	}
	if status != providerStatusThrottled {
		t.Fatalf("expected throttled, got %v", status)
	}
	// Throttled is not green, so it is not flushed as a healthy provider.
	if got := snap.healthyProviders(); len(got) != 0 {
		t.Fatalf("healthyProviders = %v, want none (throttled is not green)", got)
	}
}

func TestSnapshotCheck_UnknownStatusFailsClosed(t *testing.T) {
	dir := t.TempDir()
	// A future/garbage status string an older reader doesn't understand must
	// map to the most conservative real state (red), never silently green.
	writeHealthCache(t, dir, "zai", "quux-not-a-real-status", nowSecs())
	snap := loadProviderHealthSnapshot(dir)
	status, present := snap.check("zai")
	if !present {
		t.Fatal("expected registryPresent=true for a fresh (if unknown) entry")
	}
	if status != providerStatusRed {
		t.Fatalf("unknown status mapped to %v, want red (fail-closed-safe)", status)
	}
}

func TestProviderStatusFromString_Mapping(t *testing.T) {
	cases := map[string]providerStatus{
		"healthy":   providerStatusGreen,
		"throttled": providerStatusThrottled,
		"unhealthy": providerStatusRed,
		"":          providerStatusRed,
		"weird":     providerStatusRed,
	}
	for in, want := range cases {
		if got := providerStatusFromString(in); got != want {
			t.Errorf("providerStatusFromString(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestSnapshot_ThrottledJSONRoundTripIsAdditive confirms a file mixing the new
// throttled status with the legacy healthy/unhealthy ones decodes correctly —
// the schema extension is additive and pre-throttled writers keep working.
func TestSnapshot_ThrottledJSONRoundTripIsAdditive(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, ".gc", "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdirAll: %v", err)
	}
	data, _ := json.Marshal(map[string]any{"providers": []any{
		map[string]any{"provider": "green-prov", "status": "healthy", "probed_at": nowSecs()},
		map[string]any{"provider": "throttled-prov", "status": "throttled", "probed_at": nowSecs()},
		map[string]any{"provider": "red-prov", "status": "unhealthy", "probed_at": nowSecs()},
	}})
	if err := os.WriteFile(filepath.Join(cacheDir, "provider-health.json"), data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	snap := loadProviderHealthSnapshot(dir)
	for prov, want := range map[string]providerStatus{
		"green-prov":     providerStatusGreen,
		"throttled-prov": providerStatusThrottled,
		"red-prov":       providerStatusRed,
	} {
		got, present := snap.check(prov)
		if !present || got != want {
			t.Errorf("check(%q) = (%v, present=%v), want (%v, true)", prov, got, present, want)
		}
	}
}

// --- providerHealthGate episode-state tests ---

func TestGate_ThrottledTickAlertsOncePerEpisode(t *testing.T) {
	gate := newProviderHealthGate()
	now := time.Now()
	alerts := 0
	emit := func(_, _ string, _ time.Time, _ int) { alerts++ }

	for i := 0; i < 5; i++ {
		gate.recordThrottledTick("zai", now, emit)
	}
	if alerts != 1 {
		t.Fatalf("throttled episode: expected 1 alert, got %d", alerts)
	}

	gate.mu.Lock()
	s := gate.episodes["zai"]
	gate.mu.Unlock()
	if s.Status != providerStatusThrottled {
		t.Fatalf("episode status = %v, want throttled", s.Status)
	}
	if s.SessionCount != 5 {
		t.Fatalf("SessionCount = %d, want 5 (one per pace)", s.SessionCount)
	}

	// Recovery clears the episode; a later red opens a NEW episode → new alert.
	gate.recordGreenTick("zai")
	gate.recordRedSkip("zai", now, emit)
	if alerts != 2 {
		t.Fatalf("after green→red: expected 2 total alerts, got %d", alerts)
	}
}

// TestGate_ThrottledToRedOpensNewEpisode pins that a status change between two
// non-green states still opens a fresh episode (new alert), not a silent merge.
func TestGate_ThrottledToRedOpensNewEpisode(t *testing.T) {
	gate := newProviderHealthGate()
	now := time.Now()
	alerts := 0
	emit := func(_, _ string, _ time.Time, _ int) { alerts++ }

	gate.recordThrottledTick("zai", now, emit) // episode 1 (throttled)
	gate.recordRedSkip("zai", now, emit)       // episode 2 (red)
	if alerts != 2 {
		t.Fatalf("throttled→red expected 2 alerts (distinct episodes), got %d", alerts)
	}
}

func TestGate_NoRespawnWhileRed(t *testing.T) {
	dir := t.TempDir()
	writeHealthCache(t, dir, "zai", "unhealthy", nowSecs())
	gate := newProviderHealthGate()

	snap := loadProviderHealthSnapshot(dir)
	status, present := snap.check("zai")
	if !present || status != providerStatusRed {
		t.Fatalf("precondition: expected red present, got status=%v present=%v", status, present)
	}

	alerts := 0
	gate.recordRedSkip("zai", time.Now(), func(_, _ string, _ time.Time, _ int) { alerts++ })
	if alerts != 1 {
		t.Fatalf("expected 1 alert on first red skip, got %d", alerts)
	}
	// Second skip in same episode: no additional alert.
	gate.recordRedSkip("zai", time.Now(), func(_, _ string, _ time.Time, _ int) { alerts++ })
	if alerts != 1 {
		t.Fatalf("expected alert to fire exactly once per episode, got %d", alerts)
	}
}

func TestGate_ExactlyOneAlertPerEpisode(t *testing.T) {
	gate := newProviderHealthGate()
	now := time.Now()
	alerts := 0
	emit := func(_, _ string, _ time.Time, _ int) { alerts++ }

	// Ten red skips in episode 1.
	for i := 0; i < 10; i++ {
		gate.recordRedSkip("zai", now, emit)
	}
	if alerts != 1 {
		t.Fatalf("episode 1: expected 1 alert, got %d", alerts)
	}

	// Provider returns green → episode closes.
	gate.recordGreenTick("zai")

	// Provider goes red again → new episode → new alert.
	for i := 0; i < 5; i++ {
		gate.recordRedSkip("zai", now, emit)
	}
	if alerts != 2 {
		t.Fatalf("episode 2: expected 2 total alerts, got %d", alerts)
	}
}

func TestGate_RespawnResumesOnGreen(t *testing.T) {
	gate := newProviderHealthGate()
	now := time.Now()
	emit := func(_, _ string, _ time.Time, _ int) {}

	gate.recordRedSkip("zai", now, emit)
	// After green, episode state clears.
	gate.recordGreenTick("zai")
	gate.mu.Lock()
	s := gate.episodes["zai"]
	gate.mu.Unlock()
	if s.Status != providerStatusGreen {
		t.Fatal("expected green status after recordGreenTick")
	}
	if s.AlertSent {
		t.Fatal("expected AlertSent to be cleared after green tick")
	}
}

func TestGate_WakeBudgetNotConsumedOnRedSkip(t *testing.T) {
	// Verify that the gate produces a "continue" path — tested here by
	// checking that the SessionCount increments (i.e., the skip was
	// recorded) without emitting a second alert.
	gate := newProviderHealthGate()
	now := time.Now()
	alerts := 0
	emit := func(_, _ string, _ time.Time, count int) { alerts++; _ = count }

	gate.recordRedSkip("zai", now, emit)
	gate.recordRedSkip("zai", now, emit)
	gate.recordRedSkip("zai", now, emit)

	gate.mu.Lock()
	s := gate.episodes["zai"]
	gate.mu.Unlock()

	if s.SessionCount != 3 {
		t.Fatalf("expected SessionCount=3, got %d", s.SessionCount)
	}
	if alerts != 1 {
		t.Fatalf("expected 1 alert (not one per skip), got %d", alerts)
	}
}

func TestGate_FailOpenRegistryUnavailable(t *testing.T) {
	dir := t.TempDir() // no file
	snap := loadProviderHealthSnapshot(dir)
	status, present := snap.check("zai")
	if present {
		t.Fatal("expected registryPresent=false")
	}
	// Caller should proceed as green (fail-open); no gate interaction needed.
	if status != providerStatusGreen {
		t.Fatal("expected green (fail-open) when registry absent")
	}
}

func TestGate_NoChangeBehaviorForGreenProvider(t *testing.T) {
	dir := t.TempDir()
	writeHealthCache(t, dir, "zai", "healthy", nowSecs())
	gate := newProviderHealthGate()
	snap := loadProviderHealthSnapshot(dir)

	status, present := snap.check("zai")
	if !present || status != providerStatusGreen {
		t.Fatalf("precondition: expected green present, got status=%v present=%v", status, present)
	}
	// A green provider records a green tick; no alert.
	gate.recordGreenTick("zai")
	gate.mu.Lock()
	_, hasEpisode := gate.episodes["zai"]
	gate.mu.Unlock()
	// recordGreenTick on unknown provider is a no-op (no entry created).
	if hasEpisode {
		t.Fatal("recordGreenTick on a never-red provider should not create episode state")
	}
}

func TestGate_Concurrent(t *testing.T) {
	gate := newProviderHealthGate()
	now := time.Now()
	var mu sync.Mutex
	alerts := 0
	emit := func(_, _ string, _ time.Time, _ int) {
		mu.Lock()
		alerts++
		mu.Unlock()
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gate.recordRedSkip("zai", now, emit)
		}()
	}
	wg.Wait()

	if alerts != 1 {
		t.Fatalf("concurrent: expected exactly 1 alert, got %d", alerts)
	}
}

func TestEmitProviderHealthGateAlert_Format(t *testing.T) {
	var captured string
	w := &capWriter{fn: func(b []byte) { captured += string(b) }}
	since := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	emitProviderHealthGateAlert(nil, w, providerStatusRed, "zai", "ep-123", since, 3)

	for _, want := range []string{"zai", "ep-123", "2026-06-02T12:00:00Z", "sessions_parked=3", "status=red", "paused"} {
		if !strings.Contains(captured, want) {
			t.Errorf("alert message missing %q\ngot: %s", want, captured)
		}
	}
}

func TestEmitProviderHealthGateAlert_ThrottledFormat(t *testing.T) {
	var captured string
	w := &capWriter{fn: func(b []byte) { captured += string(b) }}
	since := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	emitProviderHealthGateAlert(nil, w, providerStatusThrottled, "zai", "ep-9", since, 2)

	for _, want := range []string{"zai", "ep-9", "status=throttled", "sessions_paced=2", "paced"} {
		if !strings.Contains(captured, want) {
			t.Errorf("throttled alert message missing %q\ngot: %s", want, captured)
		}
	}
	if strings.Contains(captured, "paused") {
		t.Errorf("throttled alert should not say 'paused'\ngot: %s", captured)
	}
}

type capWriter struct{ fn func([]byte) }

func (c *capWriter) Write(b []byte) (int, error) { c.fn(b); return len(b), nil }
