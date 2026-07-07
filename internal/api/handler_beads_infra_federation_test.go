package api

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// Landmine #13: on a split city the graph-class DAG (gcg- molecule roots, steps,
// control beads) lives in the infra store, which BeadStores() does not include.
// The HTTP ready/list handlers must federate it (or the DAG is invisible behind
// an authoritative 200), and an infra-leg hard failure is an authoritative
// failure (503), not a degraded Partial 200.

func seedInfraReadyBead(t *testing.T) (beads.Store, string) {
	t.Helper()
	graph := beads.NewMemStore()
	created, err := graph.Create(beads.Bead{Type: "task", Title: "infra graph step"})
	if err != nil {
		t.Fatalf("seed infra step: %v", err)
	}
	return graph, created.ID
}

func bodyContainsBeadID(items []beads.Bead, id string) bool {
	for _, b := range items {
		if b.ID == id {
			return true
		}
	}
	return false
}

func TestBeadReadyFederatesInfraStore(t *testing.T) {
	fs := newFakeState(t)
	graph, infraID := seedInfraReadyBead(t)
	fs.graphBeadStore = graph

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest("GET", cityURL(fs, "/beads/ready"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	var body struct {
		Items   []beads.Bead `json:"items"`
		Partial bool         `json:"partial"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	if !bodyContainsBeadID(body.Items, infraID) {
		t.Fatalf("ready items = %+v, want the infra-store graph step %q (DAG invisible?)", body.Items, infraID)
	}
	if body.Partial {
		t.Fatalf("Partial = true, want false — a healthy infra federation is authoritative")
	}
}

func TestFederatedReady_InfraPartialIsAuthoritativeFailure(t *testing.T) {
	fs := newFakeState(t)
	// A ready bead in the rig store proves we do NOT degrade to a work-only 200.
	if _, err := fs.stores["myrig"].Create(beads.Bead{Type: "task", Title: "rig work"}); err != nil {
		t.Fatalf("seed rig work: %v", err)
	}
	fs.graphBeadStore = &failingBeadStore{Store: beads.NewMemStore(), readyErr: errors.New("infra dolt unreachable")}

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest("GET", cityURL(fs, "/beads/ready"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 when the infra graph plane is unreadable (body=%q)", rec.Code, rec.Body.String())
	}
}

func TestBeadListFederatesInfraStore(t *testing.T) {
	fs := newFakeState(t)
	graph, infraID := seedInfraReadyBead(t)
	fs.graphBeadStore = graph

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest("GET", cityURL(fs, "/beads"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []beads.Bead `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	if !bodyContainsBeadID(body.Items, infraID) {
		t.Fatalf("list items = %+v, want the infra-store graph bead %q (DAG invisible?)", body.Items, infraID)
	}
}

func TestBeadListInfraLegHardFailIs503(t *testing.T) {
	fs := newFakeState(t)
	fs.graphBeadStore = &failingBeadStore{Store: beads.NewMemStore(), listErr: errors.New("infra dolt unreachable")}

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest("GET", cityURL(fs, "/beads"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 when the infra list read hard-fails (body=%q)", rec.Code, rec.Body.String())
	}
}

func TestBeadListInfraLegPartialPreservesRows(t *testing.T) {
	fs := newFakeState(t)
	base := beads.NewMemStore()
	survivor, err := base.Create(beads.Bead{Type: "task", Title: "surviving infra row"})
	if err != nil {
		t.Fatalf("seed infra survivor: %v", err)
	}
	fs.graphBeadStore = &failingBeadStore{
		Store:      base,
		listResult: []beads.Bead{mustGet(t, base, survivor.ID)},
		listErr:    &beads.PartialResultError{Op: "bd list", Err: errors.New("skipped 1 corrupt bead")},
	}

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest("GET", cityURL(fs, "/beads"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (partial rows still flow) (body=%q)", rec.Code, rec.Body.String())
	}
	var body struct {
		Items         []beads.Bead `json:"items"`
		Partial       bool         `json:"partial"`
		PartialErrors []string     `json:"partial_errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	if !body.Partial {
		t.Fatalf("Partial = false, want true (infra leg degraded but served rows)")
	}
	if !bodyContainsBeadID(body.Items, survivor.ID) {
		t.Fatalf("items = %+v, want surviving infra row %q", body.Items, survivor.ID)
	}
}

func TestBeadReadySingleStoreCityUnchanged(t *testing.T) {
	// Default fakeState: graphBeadStore nil ⇒ GraphBeadStore().Store == CityBeadStore(),
	// the infra arm never fires and behavior is byte-identical to a legacy city.
	fs := newFakeState(t)
	if _, err := fs.stores["myrig"].Create(beads.Bead{Type: "task", Title: "rig work"}); err != nil {
		t.Fatalf("seed rig work: %v", err)
	}

	h := newTestCityHandler(t, fs)
	req := httptest.NewRequest("GET", cityURL(fs, "/beads/ready"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	var body struct {
		Items   []beads.Bead `json:"items"`
		Partial bool         `json:"partial"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	if body.Partial {
		t.Fatalf("Partial = true on a single-store city, want false")
	}
	seen := map[string]bool{}
	for _, b := range body.Items {
		if seen[b.ID] {
			t.Fatalf("duplicate bead %q in single-store ready set", b.ID)
		}
		seen[b.ID] = true
	}
}

func mustGet(t *testing.T, s beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := s.Get(id)
	if err != nil {
		t.Fatalf("get %q: %v", id, err)
	}
	return b
}
