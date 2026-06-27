// provider_health_gate.go — reconciler-side provider health gate (ADR-0013 A1 M3a).
//
// Architecture: the pack's anthropic-failover-watcher writes
// provider-health.json; this file's code reads it. The reconciler is a
// pure consumer: it never classifies errors itself.
//
// Per-tick flow:
//  1. loadProviderHealthSnapshot reads the JSON file once at the start of
//     each reconciler tick and returns a providerHealthSnapshot.
//  2. snapshot.check(provider) is an in-memory lookup — no additional I/O.
//  3. providerHealthGate (reconciler-scoped, lives on CityRuntime) tracks
//     per-provider episode state across ticks so exactly one alert fires
//     per red episode.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/google/uuid"
)

const (
	// providerHealthCacheRelPath is the file written by provider-health-probe.
	// Resolved relative to cityPath so writer and reader agree without a
	// plist-injected env var.
	providerHealthCacheRelPath = ".gc/cache/provider-health.json"

	// providerHealthTTL matches the probe interval in the voxist-city pack.
	providerHealthTTL = 60 * time.Second
)

// --- snapshot (loaded once per tick) ---

// providerHealthRecord mirrors one entry in provider-health.json.
type providerHealthRecord struct {
	Provider string  `json:"provider"`
	Status   string  `json:"status"`    // "healthy" | "throttled" | "unhealthy" (unknown → red)
	ProbedAt float64 `json:"probed_at"` // Unix epoch seconds (float)
}

type providerHealthFileFormat struct {
	Providers []providerHealthRecord `json:"providers"`
}

// providerHealthSnapshot is an immutable, per-tick view of the health file.
// It is loaded once at the top of each reconciler tick via
// loadProviderHealthSnapshot; all per-session gate checks use the snapshot
// (no additional file I/O).
type providerHealthSnapshot struct {
	// present is false when the file is absent, unreadable, or empty.
	// A false present means the registry is unavailable — callers fail-open.
	present bool
	// entries maps provider name → tri-state status (green/throttled/red).
	// Only entries that exist in the file and are within TTL are stored; stale
	// or missing entries are omitted so check() reports them as not-present
	// (fail-open).
	entries map[string]providerStatus
}

// loadProviderHealthSnapshot reads cityPath/.gc/cache/provider-health.json and
// returns a snapshot for this reconciler tick. It is safe to call when the
// file is absent (returns an empty snapshot with present=false).
func loadProviderHealthSnapshot(cityPath string) *providerHealthSnapshot {
	cachePath := filepath.Join(cityPath, providerHealthCacheRelPath)
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return &providerHealthSnapshot{present: false}
	}
	var f providerHealthFileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return &providerHealthSnapshot{present: false}
	}
	snap := &providerHealthSnapshot{
		present: len(f.Providers) > 0,
		entries: make(map[string]providerStatus, len(f.Providers)),
	}
	nowSecs := float64(time.Now().UnixNano()) / 1e9
	for _, rec := range f.Providers {
		ageSecs := nowSecs - rec.ProbedAt
		if ageSecs > providerHealthTTL.Seconds() {
			// Stale — omit so check() fails-open for this provider.
			continue
		}
		snap.entries[rec.Provider] = providerStatusFromString(rec.Status)
	}
	return snap
}

// providerStatusFromString maps a provider-health.json status string to the
// tri-state enum. "healthy"→green, "throttled"→throttled (rate-limited but
// usable), "unhealthy"→red. Any unrecognized value (including future-writer
// strings older readers don't know, or malformed entries) maps to red — the
// most conservative real state — so an unknown status never silently respawns
// into a degraded provider. The mapping is additive: pre-throttled writers
// that only emit healthy/unhealthy keep working unchanged.
func providerStatusFromString(status string) providerStatus {
	switch status {
	case "healthy":
		return providerStatusGreen
	case "throttled":
		return providerStatusThrottled
	default:
		return providerStatusRed
	}
}

// check returns (status, registryPresent).
//   - registryPresent=false: file absent, unreadable, or no fresh entry for
//     providerName. Callers MUST treat this as green (fail-open).
//   - registryPresent=true: status is the provider's fresh tri-state — green
//     (respawn freely), throttled (respawn but paced), or red (gate respawn).
func (s *providerHealthSnapshot) check(providerName string) (status providerStatus, registryPresent bool) {
	if s == nil || !s.present {
		return providerStatusGreen, false
	}
	v, ok := s.entries[providerName]
	if !ok {
		return providerStatusGreen, false // no fresh entry → fail-open
	}
	return v, true
}

// healthyProviders returns all provider names that are confirmed green in this
// snapshot. Used to flush green ticks after the Phase-2 loop so a recovered
// provider's red/throttled episode state is cleared.
func (s *providerHealthSnapshot) healthyProviders() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.entries))
	for p, status := range s.entries {
		if status == providerStatusGreen {
			out = append(out, p)
		}
	}
	return out
}

// --- episode state (lives across ticks on CityRuntime) ---

type providerStatus int

const (
	providerStatusGreen providerStatus = iota
	// providerStatusThrottled — provider is rate-limited but credentials are
	// valid: respawns are allowed but paced (staggered) rather than skipped.
	providerStatusThrottled
	// providerStatusRed — provider is unhealthy/unreachable: respawns are
	// skipped entirely until it recovers. Highest (most conservative) value so
	// unknown statuses map here.
	providerStatusRed
)

// providerEpisodeState tracks one provider's non-green (red or throttled)
// episode. A new episode begins on each transition into a different status;
// AlertSent resets on green so the next episode fires a fresh alert.
type providerEpisodeState struct {
	Status    providerStatus
	EpisodeID string
	AlertSent bool
	// Since is the start of the current non-green episode (red or throttled).
	Since        time.Time
	SessionCount int
}

// providerHealthGate is reconciler-scoped, persistent across ticks.
// It lives on CityRuntime alongside sessionDrains.
type providerHealthGate struct {
	mu       sync.Mutex
	episodes map[string]*providerEpisodeState
}

// newProviderHealthGate allocates a gate with empty episode state.
func newProviderHealthGate() *providerHealthGate {
	return &providerHealthGate{episodes: make(map[string]*providerEpisodeState)}
}

// recordRedSkip notes that providerName is red this tick and a session respawn
// was parked. emitAlert is called exactly once per episode (idempotent guard
// on AlertSent). Safe to call concurrently.
func (g *providerHealthGate) recordRedSkip(
	providerName string,
	now time.Time,
	emitAlert func(provider, episodeID string, since time.Time, sessionCount int),
) {
	g.recordNonGreenEpisode(providerName, providerStatusRed, now, emitAlert)
}

// recordThrottledTick notes that providerName is throttled this tick and a
// session respawn was paced (staggered, not skipped). Like recordRedSkip it
// fires emitAlert exactly once per episode. Safe to call concurrently.
func (g *providerHealthGate) recordThrottledTick(
	providerName string,
	now time.Time,
	emitAlert func(provider, episodeID string, since time.Time, sessionCount int),
) {
	g.recordNonGreenEpisode(providerName, providerStatusThrottled, now, emitAlert)
}

// recordNonGreenEpisode tracks a red or throttled episode for providerName.
// Entering a status different from the current one opens a fresh episode (new
// ID, reset alert, Since=now, count=1); staying in the same status increments
// the parked/paced session count. emitAlert fires exactly once per episode.
func (g *providerHealthGate) recordNonGreenEpisode(
	providerName string,
	status providerStatus,
	now time.Time,
	emitAlert func(provider, episodeID string, since time.Time, sessionCount int),
) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.episodeFor(providerName)
	if s.Status != status {
		s.Status = status
		s.EpisodeID = uuid.New().String()
		s.AlertSent = false
		s.Since = now
		s.SessionCount = 1
	} else {
		s.SessionCount++
	}
	if !s.AlertSent {
		emitAlert(providerName, s.EpisodeID, s.Since, s.SessionCount)
		s.AlertSent = true
	}
}

// recordGreenTick clears non-green state so the next red/throttled transition
// opens a fresh episode and fires a new alert. Called once per tick per green
// provider.
func (g *providerHealthGate) recordGreenTick(providerName string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if s, ok := g.episodes[providerName]; ok {
		s.Status = providerStatusGreen
		s.AlertSent = false
		s.SessionCount = 0
	}
}

func (g *providerHealthGate) episodeFor(providerName string) *providerEpisodeState {
	if s, ok := g.episodes[providerName]; ok {
		return s
	}
	s := &providerEpisodeState{}
	g.episodes[providerName] = s
	return s
}

// --- alert emission ---

// emitProviderHealthGateAlert writes the ADR-0013 escalation alert to stdout
// and records a ProviderHealthGateAlert event. Called once per episode. status
// selects the wording: RED parks respawns ("paused"); THROTTLED keeps respawning
// but staggers them ("paced"). Any other status falls back to the RED wording.
func emitProviderHealthGateAlert(
	rec events.Recorder,
	stdout io.Writer,
	status providerStatus,
	provider, episodeID string,
	since time.Time,
	sessionCount int,
) {
	action := fmt.Sprintf("Respawn for %s sessions paused", provider)
	statusLabel := "red"
	countLabel := "sessions_parked"
	if status == providerStatusThrottled {
		action = fmt.Sprintf("Respawn for %s sessions paced (staggered to avoid a synchronized retry storm)", provider)
		statusLabel = "throttled"
		countLabel = "sessions_paced"
	}
	msg := fmt.Sprintf(
		"Provider health gate OPEN: provider=%s status=%s episode=%s since=%s %s=%d. "+
			"%s. "+
			"Resume is automatic when provider returns green. "+
			"Verify token with: gc provider status %s",
		provider, statusLabel, episodeID, since.UTC().Format(time.RFC3339), countLabel, sessionCount,
		action, provider,
	)
	fmt.Fprintln(stdout, msg) //nolint:errcheck
	if rec == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"provider":      provider,
		"status":        statusLabel,
		"episode_id":    episodeID,
		"since":         since.UTC().Format(time.RFC3339),
		"session_count": sessionCount,
	})
	rec.Record(events.Event{
		Type:    events.ProviderHealthGateAlert,
		Ts:      time.Now().UTC(),
		Actor:   "gc",
		Subject: provider,
		Message: msg,
		Payload: payload,
	})
}
