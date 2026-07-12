package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	beadslib "github.com/steveyegge/beads"
)

type nativeDoltFactoryStorage struct {
	beadslib.Storage

	mu         sync.Mutex
	issues     []*beadslib.Issue
	readyCalls int
}

func (s *nativeDoltFactoryStorage) CreateIssue(_ context.Context, issue *beadslib.Issue, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	if issue.Status == "" {
		issue.Status = beadslib.StatusOpen
	}
	if issue.IssueType == "" {
		issue.IssueType = beadslib.TypeTask
	}
	if issue.CreatedAt.IsZero() {
		issue.CreatedAt = now
	}
	if issue.UpdatedAt.IsZero() {
		issue.UpdatedAt = now
	}
	s.issues = append(s.issues, cloneNativeDoltFactoryIssue(issue))
	return nil
}

func (s *nativeDoltFactoryStorage) SearchIssues(_ context.Context, _ string, _ beadslib.IssueFilter) ([]*beadslib.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneNativeDoltFactoryIssues(s.issues), nil
}

func (s *nativeDoltFactoryStorage) GetReadyWork(_ context.Context, filter beadslib.WorkFilter) ([]*beadslib.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.readyCalls++
	ready := make([]*beadslib.Issue, 0, len(s.issues))
	for _, issue := range s.issues {
		if issue.Status != filter.Status {
			continue
		}
		if filter.Assignee != nil && issue.Assignee != *filter.Assignee {
			continue
		}
		ready = append(ready, cloneNativeDoltFactoryIssue(issue))
	}
	return ready, nil
}

func (s *nativeDoltFactoryStorage) Close() error { return nil }

func (s *nativeDoltFactoryStorage) readyCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readyCalls
}

func (s *nativeDoltFactoryStorage) resetReadyCallCount() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readyCalls = 0
}

func cloneNativeDoltFactoryIssues(src []*beadslib.Issue) []*beadslib.Issue {
	dst := make([]*beadslib.Issue, 0, len(src))
	for _, issue := range src {
		dst = append(dst, cloneNativeDoltFactoryIssue(issue))
	}
	return dst
}

func cloneNativeDoltFactoryIssue(src *beadslib.Issue) *beadslib.Issue {
	if src == nil {
		return nil
	}
	dst := *src
	dst.Labels = append([]string(nil), src.Labels...)
	dst.Metadata = append(dst.Metadata[:0:0], src.Metadata...)
	dst.Dependencies = append([]*beadslib.Dependency(nil), src.Dependencies...)
	return &dst
}

type nativeDoltFactoryRuntime struct {
	runtime.Provider
}

func (*nativeDoltFactoryRuntime) IsRunning(string) bool { return false }

func TestOpenStoreAtForCityEligibleNativeDoltCompletesCoherentDesiredStatePass(t *testing.T) {
	t.Setenv("GC_BEADS_FORCE_FALLBACK", "")
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "rig-A")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatalf("create rig scope: %v", err)
	}
	storage := &nativeDoltFactoryStorage{}
	selectedNative := beads.NewNativeDoltStoreWithStorageForTesting(storage)
	var identityProbeCalls, identityDeferralCalls, nativeOpenCalls, fallbackOpenCalls int
	checker := nativeDoltFactoryPassingPreflight(rigPath, &identityProbeCalls, &identityDeferralCalls)

	opened, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot:        rigPath,
		CityPath:         cityPath,
		Provider:         "bd",
		PreflightChecker: checker,
		OpenBdStore: func() (beads.Store, error) {
			fallbackOpenCalls++
			t.Fatal("OpenBdStore called for a native-eligible hosted scope")
			return nil, nil
		},
		OpenNativeStore: func() (beads.Store, error) {
			nativeOpenCalls++
			return selectedNative, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity: %v", err)
	}
	native, ok := opened.Store.(*beads.NativeDoltStore)
	if !ok {
		t.Fatalf("selected store = %T, want exact *beads.NativeDoltStore", opened.Store)
	}
	if native != selectedNative {
		t.Fatalf("selected NativeDoltStore pointer = %p, want opener result %p", native, selectedNative)
	}
	if identityProbeCalls != 1 || identityDeferralCalls != 1 {
		t.Fatalf("hosted identity callbacks = direct:%d deferred:%d, want 1 each", identityProbeCalls, identityDeferralCalls)
	}
	if nativeOpenCalls != 1 || fallbackOpenCalls != 0 {
		t.Fatalf("factory opener calls = native:%d fallback:%d, want native:1 fallback:0", nativeOpenCalls, fallbackOpenCalls)
	}
	t.Cleanup(func() {
		if err := native.CloseStore(); err != nil {
			t.Fatalf("close selected NativeDoltStore: %v", err)
		}
	})
	if opened.Diagnostic.Store != beads.BeadsStoreNameNativeDoltStore || !opened.Diagnostic.NativeStoreEligible {
		t.Fatalf("native diagnostic = %+v, want eligible %q", opened.Diagnostic, beads.BeadsStoreNameNativeDoltStore)
	}
	if opened.Diagnostic.PreflightGate != "" || opened.Diagnostic.PreflightReason != "" {
		t.Fatalf("eligible diagnostic retained failure detail: %+v", opened.Diagnostic)
	}

	const template = "rig-A/planner"
	if _, err := opened.Store.Create(beads.Bead{
		ID:     "native-routed-work",
		Title:  "native routed work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": template,
		},
	}); err != nil {
		t.Fatalf("seed selected NativeDoltStore: %v", err)
	}
	if _, err := native.Ready(beads.ReadyQuery{TierMode: beads.TierBoth}); err != nil {
		t.Fatalf("calibrate selected NativeDoltStore Ready: %v", err)
	}
	oneLogicalReadyCost := storage.readyCallCount()
	if oneLogicalReadyCost == 0 {
		t.Fatal("selected NativeDoltStore Ready did not reach backing storage")
	}
	storage.resetReadyCallCount()

	minSessions, maxSessions := 0, 2
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "planner",
			Dir:               "rig-A",
			Provider:          "mock",
			MinActiveSessions: &minSessions,
			MaxActiveSessions: &maxSessions,
		}},
		Rigs: []config.Rig{{Name: "rig-A", Path: rigPath}},
		Providers: map[string]config.ProviderSpec{
			"mock": {Command: "true"},
		},
	}
	var stderr bytes.Buffer
	result := buildDesiredStateWithSessionBeads(
		"test-city",
		cityPath,
		time.Now(),
		cfg,
		&nativeDoltFactoryRuntime{},
		beads.NewMemStore(),
		map[string]beads.Store{"rig-A": opened.Store},
		newSessionBeadSnapshot(nil),
		nil,
		&stderr,
	)
	if result.StoreQueryPartial {
		t.Fatalf("desired-state pass was partial; stderr=%s", stderr.String())
	}
	if got := result.ScaleCheckCounts[template]; got != 1 {
		t.Fatalf("scale-from-zero demand = %d, want 1; stderr=%s", got, stderr.String())
	}
	if got := len(result.State); got != 1 {
		t.Fatalf("desired sessions = %d, want 1; state=%v stderr=%s", got, result.State, stderr.String())
	}
	persisted, err := native.Get("native-routed-work")
	if err != nil {
		t.Fatalf("read routed work after desired-state pass: %v", err)
	}
	if got := persisted.Metadata["gc.routed_to"]; got != template {
		t.Fatalf("persisted route = %q, want coherent canonical route %q", got, template)
	}
	if got := storage.readyCallCount(); got != oneLogicalReadyCost {
		t.Fatalf("selected NativeDoltStore backing Ready calls = %d, want one logical Ready cost %d for one coherent untouched-store generation", got, oneLogicalReadyCost)
	}
}

func nativeDoltFactoryPassingPreflight(scope string, identityProbeCalls, identityDeferralCalls *int) contract.PreflightChecker {
	files := fsys.NewFake()
	files.Dirs[filepath.Join(scope, ".beads")] = true
	files.Files[filepath.Join(scope, ".beads", "metadata.json")] = []byte(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "bd_prj_c069247fbac36e2b",
		"project_id": "prj_c069247fbac36e2b"
	}`)
	return contract.PreflightChecker{
		FS:                  files,
		Provider:            "bd",
		BeadsLibraryVersion: "1.1.0",
		BDContext: func(string) (contract.PreflightBDContext, error) {
			return contract.PreflightBDContext{
				Backend:       "dolt",
				DoltMode:      "server",
				BDVersion:     "1.1.0",
				SchemaVersion: 1,
			}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			(*identityProbeCalls)++
			return "", false, errors.New("dial hosted gateway: access denied")
		},
		DeferIdentityToNativeOpen: func(gotScope string) bool {
			(*identityDeferralCalls)++
			return gotScope == scope
		},
	}
}
