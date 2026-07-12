package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

// molGraphJSON is the federated bead-graph payload the controller returns for a
// molecule whose step beads live in the infra store: root gcg-1 with a closed
// step (gcg-2) and an in_progress step (gcg-3).
func molGraphJSON() map[string]any {
	return map[string]any{
		"root": map[string]any{"id": "gcg-1", "title": "workflow", "status": "open"},
		"beads": []map[string]any{
			{"id": "gcg-1", "title": "workflow", "status": "open"},
			{"id": "gcg-2", "title": "step one", "status": "closed"},
			// gcg-3 carries the member metadata the finalize human-approval read
			// depends on — the whole reason this federation fix exists is that this
			// value must survive from the infra-store step into steps[].issue.metadata.
			{"id": "gcg-3", "title": "step two", "status": "in_progress", "metadata": map[string]any{"human.approval": "auto"}},
		},
		"deps": []map[string]any{},
	}
}

func molGraphServer(t *testing.T, capture func(method, path string)) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			capture(r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(molGraphJSON()) //nolint:errcheck
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestBdMolRoutable covers the routable read shapes and the forms that must not
// route (other subcommands, omitted id, view flags). Ported from the proven X2
// fix (commit 305bed90d); args are the tokens after "mol".
func TestBdMolRoutable(t *testing.T) {
	cases := []struct {
		args []string
		ok   bool
	}{
		{[]string{"current", "gcg-1"}, true},
		{[]string{"progress", "gcg-1"}, true},
		{[]string{"current", "gcg-1", "--json"}, true},
		{[]string{"current"}, false},                            // id omitted (bd infers it)
		{[]string{"pour", "proto"}, false},                      // not a read subcommand
		{[]string{"current", "--for", "agent", "gcg-1"}, false}, // view flag: not routable
		{nil, false},
	}
	for _, tc := range cases {
		if got := bdMolRoutableArgs(tc.args); got != tc.ok {
			t.Errorf("bdMolRoutableArgs(%v) = %v, want %v", tc.args, got, tc.ok)
		}
	}
}

// TestDispatchBdMolViaAPICurrent proves `mol current <id>` fetches the federated
// graph via GET /v0/city/{city}/beads/graph/{id} and renders step status
// indicators from the returned topology (the routed source reaches the
// infra-store-resident steps the single-store bd cannot see).
func TestDispatchBdMolViaAPICurrent(t *testing.T) {
	var gotMethod, gotPath string
	ts := molGraphServer(t, func(m, p string) { gotMethod, gotPath = m, p })
	client := api.NewCityScopedClient(ts.URL, "alpha")

	t.Run("text", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := dispatchBdMolViaAPI(client, "current", "gcg-1", false, &out, &errb); code != 0 {
			t.Fatalf("mol current: code=%d err=%s", code, errb.String())
		}
		if gotMethod != http.MethodGet || gotPath != "/v0/city/alpha/beads/graph/gcg-1" {
			t.Fatalf("mol -> %s %s, want GET /v0/city/alpha/beads/graph/gcg-1", gotMethod, gotPath)
		}
		o := out.String()
		if !strings.Contains(o, "[done] gcg-2") || !strings.Contains(o, "[current] gcg-3") {
			t.Fatalf("mol current render = %q, want [done] gcg-2 + [current] gcg-3 (root excluded)", o)
		}
		if strings.Contains(o, "] gcg-1") {
			t.Fatalf("mol current rendered the root as a step: %q", o)
		}
	})

	t.Run("json steps are federated, not null", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := dispatchBdMolViaAPI(client, "current", "gcg-1", true, &out, &errb); code != 0 {
			t.Fatalf("mol current --json: code=%d err=%s", code, errb.String())
		}
		var got []molProgressJSON
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("mol current --json output is not a molecule-progress array: %v\nraw: %s", err, out.String())
		}
		if len(got) != 1 {
			t.Fatalf("mol current --json len=%d, want 1 molecule", len(got))
		}
		mp := got[0]
		if mp.Steps == nil {
			t.Fatal("mol current --json steps is null on a split city (the bug this fix closes)")
		}
		if mp.MoleculeID != "gcg-1" || len(mp.Steps) != 2 {
			t.Fatalf("mol current --json = %+v, want molecule_id gcg-1 with 2 steps", mp)
		}
		if mp.Completed != 1 || mp.Total != 2 {
			t.Fatalf("mol current --json progress = %d/%d, want 1/2", mp.Completed, mp.Total)
		}
		byID := map[string]molStepJSON{}
		for _, s := range mp.Steps {
			byID[s.Issue.ID] = s
		}
		if byID["gcg-2"].Status != "done" {
			t.Errorf("gcg-2 status = %q, want done", byID["gcg-2"].Status)
		}
		if byID["gcg-3"].Status != "current" || !byID["gcg-3"].IsCurrent {
			t.Errorf("gcg-3 = %+v, want status=current is_current=true", byID["gcg-3"])
		}
		// The load-bearing round-trip: a step's metadata (here the finalize
		// human-approval value) must federate from the infra-store bead all the
		// way into steps[].issue.metadata, or the pack still cannot read approval.
		if got := byID["gcg-3"].Issue.Metadata["human.approval"]; got != "auto" {
			t.Errorf("gcg-3 issue.metadata[human.approval] = %q, want \"auto\" (federated member metadata lost)", got)
		}
	})
}

// TestDispatchBdMolViaAPIProgress proves `mol progress <id>` renders the counts
// summary in both text and JSON from the federated graph.
func TestDispatchBdMolViaAPIProgress(t *testing.T) {
	ts := molGraphServer(t, nil)
	client := api.NewCityScopedClient(ts.URL, "alpha")

	t.Run("text", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := dispatchBdMolViaAPI(client, "progress", "gcg-1", false, &out, &errb); code != 0 {
			t.Fatalf("mol progress: code=%d err=%s", code, errb.String())
		}
		if !strings.Contains(out.String(), "1/2 steps complete (50%)") {
			t.Fatalf("mol progress render = %q, want 1/2 steps complete (50%%)", out.String())
		}
	})

	t.Run("json", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := dispatchBdMolViaAPI(client, "progress", "gcg-1", true, &out, &errb); code != 0 {
			t.Fatalf("mol progress --json: code=%d err=%s", code, errb.String())
		}
		var got molProgressSummaryJSON
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("mol progress --json not decodable: %v\nraw: %s", err, out.String())
		}
		if got.MoleculeID != "gcg-1" || got.Total != 2 || got.Completed != 1 || got.InProgress != 1 {
			t.Fatalf("mol progress --json = %+v, want gcg-1 total=2 completed=1 in_progress=1", got)
		}
		if got.CurrentStepID != "gcg-3" {
			t.Errorf("current_step_id = %q, want gcg-3", got.CurrentStepID)
		}
		if got.Percent == nil || *got.Percent != 50 {
			t.Errorf("percent = %v, want 50", got.Percent)
		}
	})
}

// TestMaybeRouteBdMolViaAPI proves the doBd gate: a split city routes
// `mol current --json` to the federated graph (non-null steps), while a
// single-store city, a non-routable mol form, and a controller-down city all
// fall through to the single-store passthrough.
func TestMaybeRouteBdMolViaAPI(t *testing.T) {
	ts := molGraphServer(t, nil)
	restore := bdMolAPIClient
	bdMolAPIClient = func(string) *api.Client { return api.NewCityScopedClient(ts.URL, "alpha") }
	t.Cleanup(func() { bdMolAPIClient = restore })

	splitCity := filepath.Join(t.TempDir(), "split")
	seedSplitCityInfraMarker(t, splitCity)
	singleCity := filepath.Join(t.TempDir(), "single")

	t.Run("split city handles and federates steps", func(t *testing.T) {
		var out, errb bytes.Buffer
		code, handled := maybeRouteBdMolViaAPI(splitCity, []string{"mol", "current", "gcg-1", "--json"}, &out, &errb)
		if !handled {
			t.Fatal("split city should route mol current through the controller graph")
		}
		if code != 0 {
			t.Fatalf("code=%d err=%s", code, errb.String())
		}
		var got []molProgressJSON
		if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got) != 1 || got[0].Steps == nil {
			t.Fatalf("federated mol current --json = %q (err=%v), want array with non-null steps", out.String(), err)
		}
		if len(got[0].Steps) != 2 {
			t.Fatalf("federated steps len=%d, want 2", len(got[0].Steps))
		}
	})

	t.Run("single-store city falls through to passthrough", func(t *testing.T) {
		var out, errb bytes.Buffer
		_, handled := maybeRouteBdMolViaAPI(singleCity, []string{"mol", "current", "gcg-1", "--json"}, &out, &errb)
		if handled {
			t.Fatal("single-store city must use the byte-identical single-store passthrough")
		}
	})

	t.Run("non-routable mol form falls through", func(t *testing.T) {
		var out, errb bytes.Buffer
		if _, handled := maybeRouteBdMolViaAPI(splitCity, []string{"mol", "pour", "proto"}, &out, &errb); handled {
			t.Fatal("non-read mol subcommand must not route")
		}
	})

	t.Run("controller down falls through", func(t *testing.T) {
		down := bdMolAPIClient
		bdMolAPIClient = func(string) *api.Client { return nil }
		t.Cleanup(func() { bdMolAPIClient = down })
		var out, errb bytes.Buffer
		if _, handled := maybeRouteBdMolViaAPI(splitCity, []string{"mol", "current", "gcg-1"}, &out, &errb); handled {
			t.Fatal("controller-down split city must fall back to the passthrough, not hard-fail")
		}
	})
}
