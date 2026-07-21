package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func TestV2RoutedToNamespaceCheckWarnsOnShortBoundRoutes(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "dog", BindingName: "gastown"},
			{Name: "polecat", Dir: "repo", BindingName: "gastown"},
		},
		Rigs: []config.Rig{
			{Name: "repo", Path: rigDir},
		},
	}
	cityStore := &routeQuerySpyStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "warrant", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "dog"}},
	}, nil)}
	rigStore := &routeQuerySpyStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "RIG-1", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "repo/polecat"}},
	}, nil)}
	stores := map[string]beads.Store{
		cityDir: cityStore,
		rigDir:  rigStore,
	}

	result := newV2RoutedToNamespaceCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		store, ok := stores[path]
		if !ok {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		`city bead CITY-1 has gc.routed_to="dog"; use "gastown.dog"`,
		`rig repo bead RIG-1 has gc.routed_to="repo/polecat"; use "repo/gastown.polecat"`,
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
	for scope, store := range map[string]*routeQuerySpyStore{"city": cityStore, "rig": rigStore} {
		if len(store.queries) != 1 {
			t.Errorf("%s List calls = %d, want 1", scope, len(store.queries))
		}
	}
}

func TestV2RoutedToNamespaceCheckScansScopeOnceWithoutLabels(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{}
	beadsForStore := make([]beads.Bead, 0, 1_000)
	for i := range 100 {
		cfg.Agents = append(cfg.Agents, config.Agent{Name: fmt.Sprintf("agent-%03d", i), BindingName: "gastown"})
	}
	for i := range 1_000 {
		beadsForStore = append(beadsForStore, beads.Bead{
			ID:       fmt.Sprintf("CITY-%d", i),
			Title:    "work",
			Type:     "task",
			Status:   "open",
			Metadata: map[string]string{"gc.routed_to": fmt.Sprintf("agent-%03d", i%100)},
		})
	}
	store := &routeQuerySpyStore{Store: beads.NewMemStoreFrom(0, beadsForStore, nil)}

	result := newV2RoutedToNamespaceCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	if len(store.queries) != 1 {
		t.Fatalf("List calls = %d, want 1", len(store.queries))
	}
	query := store.queries[0]
	if !query.AllowScan {
		t.Fatalf("query %+v AllowScan = false, want true", query)
	}
	if !query.SkipLabels {
		t.Fatalf("query %+v SkipLabels = false, want true", query)
	}
	if len(query.Metadata) != 0 {
		t.Fatalf("query %+v Metadata = %#v, want no metadata filter", query, query.Metadata)
	}
}

func TestV2RoutedToNamespaceCheckAllowsCanonicalRoutes(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "dog", BindingName: "gastown"},
			{Name: "human"},
		},
	}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "warrant", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "gastown.dog"}},
		{ID: "CITY-2", Title: "human", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "human"}},
	}, nil)

	result := newV2RoutedToNamespaceCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok: %#v", result.Status, result)
	}
}

func TestV2RoutedToNamespaceCheckWarnsOnBoundNamedSessionShortRoutes(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		NamedSessions: []config.NamedSession{
			{Name: "mayor", BindingName: "gastown"},
		},
	}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "mail", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "mayor"}},
	}, nil)

	result := newV2RoutedToNamespaceCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	want := `city bead CITY-1 has gc.routed_to="mayor"; use "gastown.mayor"`
	if !strings.Contains(details, want) {
		t.Fatalf("details missing %q:\n%s", want, details)
	}
}

func TestV2RoutedToNamespaceCheckAllowsAmbiguousShortRouteForUnboundAgent(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "dog"},
			{Name: "dog", BindingName: "gastown"},
		},
	}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "warrant", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "dog"}},
	}, nil)

	result := newV2RoutedToNamespaceCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok: %#v", result.Status, result)
	}
}

func TestV2RoutedToNamespaceCheckWarnsOnSkippedStoreScopes(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "dog", BindingName: "gastown"},
		},
		Rigs: []config.Rig{
			{Name: "repo", Path: rigDir},
		},
	}

	result := newV2RoutedToNamespaceCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		switch path {
		case cityDir:
			return nil, errors.New("city offline")
		case rigDir:
			return routeListErrorStore{err: errors.New("rig offline")}, nil
		default:
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		"city skipped: opening bead store: city offline",
		"rig repo skipped: listing beads: rig offline",
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
}

type routeListErrorStore struct {
	beads.Store
	err error
}

func (s routeListErrorStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, s.err
}

type routeQuerySpyStore struct {
	beads.Store
	queries []beads.ListQuery
}

func (s *routeQuerySpyStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, query)
	return s.Store.List(query)
}
