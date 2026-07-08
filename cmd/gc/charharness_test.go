package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/chartest"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

// charharness is the cmd/gc glue of the three-lane characterization harness. It
// drives a read command's route<X> seam across the remote / local-controller-
// alive / serverless lanes and hands the captured surface to internal/chartest
// for canonicalization + golden comparison. See engdocs/plans/cli-unification/
// HARNESS-DESIGN.md.

// charLane is one of the three routing lanes.
type charLane struct {
	name      string
	client    *api.Client   // nil for the serverless lane
	nilReason string        // consulted only when client == nil
	reqs      *atomic.Int64 // server-side request counter; nil for serverless
}

type charHarness struct {
	cityPath string
	cs       *controllerState
}

// newCharHarness builds a throwaway file-store city seeded with the given
// convoys (on disk, before the server exists, so all three lanes read one set).
func newCharHarness(t *testing.T, convoyTitles ...string) *charHarness {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_DEBUG", "1") // the route=/reason= stderr line is gated on this

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"chartest-city\"\nprefix = \"gc\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	seed, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	for _, title := range convoyTitles {
		if _, err := seed.Create(beads.Bead{Title: title, Type: "convoy"}); err != nil {
			t.Fatalf("seed convoy %q: %v", title, err)
		}
	}

	cfg, err := loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("load cfg: %v", err)
	}
	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "chartest-city", cityPath)
	return &charHarness{cityPath: cityPath, cs: cs}
}

// lanes stands up one in-process server (plain + TLS fronts) shared by the two
// client lanes and returns all three lanes. Both fronts wrap the identical mux
// over the same controllerState, so every lane reads one store.
func (h *charHarness) lanes(t *testing.T) []charLane {
	t.Helper()
	base := api.NewSupervisorMux(&singleCityStateResolver{state: h.cs}, nil, false, "controller", "test", time.Now()).
		WithAnyHostAllowed().
		Handler()

	var aliveReqs, tlsReqs atomic.Int64
	aliveSrv := httptest.NewServer(countingHandler(&aliveReqs, base))
	t.Cleanup(aliveSrv.Close)
	tlsSrv := httptest.NewTLSServer(countingHandler(&tlsReqs, base))
	t.Cleanup(tlsSrv.Close)

	caPath := writeCapstoneServerCA(t, tlsSrv)
	remoteClient, err := api.NewRemoteCityScopedClient(tlsSrv.URL, "chartest-city", api.RemoteOptions{CAFile: caPath})
	if err != nil {
		t.Fatalf("remote client: %v", err)
	}
	return []charLane{
		{name: "remote", client: remoteClient, reqs: &tlsReqs},
		{name: "alive", client: api.NewCityScopedClient(aliveSrv.URL, "chartest-city"), reqs: &aliveReqs},
		{name: "serverless", client: nil, nilReason: "controller-down"},
	}
}

func countingHandler(counter *atomic.Int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.Add(1)
		next.ServeHTTP(w, r)
	})
}

// captureLane drives routeConvoyList for one lane (human + json runs), reads the
// store back, and canonicalizes every surface with one Canonicalizer so bead
// ids stay identical across stdout/json/store within the lane.
func (h *charHarness) captureLane(t *testing.T, lane charLane) chartest.Capture {
	t.Helper()

	var ho, he bytes.Buffer
	var before int64
	if lane.reqs != nil {
		before = lane.reqs.Load()
	}
	exit := routeConvoyList(h.cityPath, lane.client, lane.nilReason, false, &ho, &he)
	var reqDelta int64
	if lane.reqs != nil {
		reqDelta = lane.reqs.Load() - before
	}

	var jo, je bytes.Buffer
	_ = routeConvoyList(h.cityPath, lane.client, lane.nilReason, true, &jo, &je)

	store, err := openCityStoreAt(h.cityPath)
	if err != nil {
		t.Fatalf("readback open: %v", err)
	}
	convoys, err := store.List(beads.ListQuery{Type: "convoy", IncludeClosed: true, Live: true})
	if err != nil {
		t.Fatalf("readback list: %v", err)
	}
	sort.Slice(convoys, func(i, j int) bool { return convoys[i].ID < convoys[j].ID })
	storeLines := make([]string, len(convoys))
	for i, b := range convoys {
		storeLines[i] = fmt.Sprintf("%s type=%s status=%s title=%q", b.ID, b.Type, b.Status, b.Title)
	}

	var eventLines []string
	if lane.client != nil {
		if fake, ok := h.cs.EventProvider().(*events.Fake); ok {
			evs, _ := fake.List(events.Filter{})
			for _, e := range evs {
				eventLines = append(eventLines, fmt.Sprintf("type=%s subject=%s", e.Type, e.Subject))
			}
			sort.Strings(eventLines)
		}
	}

	c := chartest.NewCanonicalizer(chartest.DefaultRules()...)
	return chartest.Capture{
		Exit:          exit,
		Stdout:        c.Canonicalize(ho.Bytes()),
		JSON:          c.Canonicalize(jo.Bytes()),
		StoreReadback: canonLines(c, storeLines),
		Events:        canonLines(c, eventLines),
		Stderr:        c.Canonicalize(he.Bytes()),
		Counts:        []chartest.Count{{Name: "api_requests", N: int(reqDelta)}},
	}
}

func canonLines(c *chartest.Canonicalizer, lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = string(c.Canonicalize([]byte(l)))
	}
	return out
}
