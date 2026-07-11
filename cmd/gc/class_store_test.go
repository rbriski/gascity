package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// controllerClassAccessor names a controllerState per-class accessor for the
// identity conformance table.
type controllerClassAccessor struct {
	name string
	got  func(cs *controllerState) beads.Store
}

var controllerCityClassAccessors = []controllerClassAccessor{
	// graphBeadStore returns the strongly-typed beads.GraphStore; unwrap its
	// embedded .Store so the identity check compares the underlying store pointer.
	{"graphBeadStore", func(cs *controllerState) beads.Store { return cs.graphBeadStore().Store }},
	// sessionsBeadStore returns the strongly-typed beads.SessionStore; unwrap its
	// embedded .Store so the identity check compares the underlying store pointer.
	{"sessionsBeadStore", func(cs *controllerState) beads.Store { return cs.sessionsBeadStore().Store }},
	// mailBeadStore returns the strongly-typed beads.MailStore; unwrap its
	// embedded .Store so the identity check compares the underlying store pointer.
	{"mailBeadStore", func(cs *controllerState) beads.Store { return cs.mailBeadStore().Store }},
	// nudgesBeadStore returns the strongly-typed beads.NudgesStore; unwrap its
	// embedded .Store so the identity check compares the underlying store pointer.
	{"nudgesBeadStore", func(cs *controllerState) beads.Store { return cs.nudgesBeadStore().Store }},
	// ordersBeadStore returns the strongly-typed beads.OrdersStore; unwrap its
	// embedded .Store so the identity check compares the underlying store pointer.
	{"ordersBeadStore", func(cs *controllerState) beads.Store { return cs.ordersBeadStore("").Store }},
	// cityWorkStore returns the strongly-typed beads.WorkStore; unwrap its embedded
	// .Store so the identity check compares the underlying store pointer.
	{"cityWorkStore", func(cs *controllerState) beads.Store { return cs.cityWorkStore().Store }},
}

// TestControllerStateClassAccessorsAreIdentity pins that every controllerState
// per-class accessor returns the exact same pointer the call site uses today:
// CityBeadStore() for the city-resident classes and BeadStores() for work.
func TestControllerStateClassAccessorsAreIdentity(t *testing.T) {
	city := beads.NewMemStore()
	rig := beads.NewMemStore()
	cs := &controllerState{
		cityName:      "test-city",
		cityBeadStore: city,
		beadStores:    map[string]beads.Store{"myrig": rig},
	}

	for _, acc := range controllerCityClassAccessors {
		if got := acc.got(cs); !sameStorePtr(got, city) {
			t.Errorf("controllerState.%s() = %p, want CityBeadStore %p", acc.name, got, city)
		}
	}

	work := cs.workBeadStores()
	want := cs.BeadStores()
	if len(work) != len(want) {
		t.Fatalf("workBeadStores() len = %d, want %d", len(work), len(want))
	}
	for name, store := range want {
		// work[name] is a strongly-typed beads.WorkStore; unwrap its embedded .Store
		// so the identity check compares the underlying store pointer.
		if !sameStorePtr(work[name].Store, store) {
			t.Errorf("workBeadStores()[%q] = %p, want %p", name, work[name].Store, store)
		}
	}
}

// infraRoutedControllerClassAccessors names the controllerState per-class
// accessors that route to the infra store on a split city: the coordination
// classes (graph, sessions, messaging, nudges, orders).
var infraRoutedControllerClassAccessors = []controllerClassAccessor{
	{"graphBeadStore", func(cs *controllerState) beads.Store { return cs.graphBeadStore().Store }},
	{"sessionsBeadStore", func(cs *controllerState) beads.Store { return cs.sessionsBeadStore().Store }},
	{"mailBeadStore", func(cs *controllerState) beads.Store { return cs.mailBeadStore().Store }},
	{"nudgesBeadStore", func(cs *controllerState) beads.Store { return cs.nudgesBeadStore().Store }},
	{"ordersBeadStore", func(cs *controllerState) beads.Store { return cs.ordersBeadStore("").Store }},
}

// TestControllerStateClassAccessorsRouteToInfraStore pins the split-city
// routing: with cityInfraStore set, every coordination-class accessor returns
// the infra pointer, while the work-class accessors (cityWorkStore,
// workBeadStores) still return the city/rig pointers.
func TestControllerStateClassAccessorsRouteToInfraStore(t *testing.T) {
	city := beads.NewMemStore()
	infra := beads.NewMemStore()
	rig := beads.NewMemStore()
	cs := &controllerState{
		cityName:       "test-city",
		cityBeadStore:  city,
		cityInfraStore: infra,
		beadStores:     map[string]beads.Store{"myrig": rig},
	}

	for _, acc := range infraRoutedControllerClassAccessors {
		if got := acc.got(cs); !sameStorePtr(got, infra) {
			t.Errorf("controllerState.%s() = %p, want cityInfraStore %p", acc.name, got, infra)
		}
	}

	// Work class stays on the city/rig stores — the boundary must not leak work
	// into the infra store.
	if got := cs.cityWorkStore().Store; !sameStorePtr(got, city) {
		t.Errorf("controllerState.cityWorkStore() = %p, want CityBeadStore %p", got, city)
	}
	work := cs.workBeadStores()
	want := cs.BeadStores()
	if len(work) != len(want) {
		t.Fatalf("workBeadStores() len = %d, want %d", len(work), len(want))
	}
	for name, store := range want {
		if !sameStorePtr(work[name].Store, store) {
			t.Errorf("workBeadStores()[%q] = %p, want %p", name, work[name].Store, store)
		}
	}
}

// TestCityRuntimeClassAccessorsAreIdentity pins that every CityRuntime per-class
// accessor returns the same pointer the runtime call site uses today.
func TestCityRuntimeClassAccessorsAreIdentity(t *testing.T) {
	city := beads.NewMemStore()
	cr := &CityRuntime{
		cityName:            "test-city",
		standaloneCityStore: city,
		standaloneRigStores: map[string]beads.Store{"myrig": beads.NewMemStore()},
	}

	accessors := []struct {
		name string
		got  func() beads.Store
	}{
		// graphBeadStore returns the strongly-typed beads.GraphStore; unwrap its
		// embedded .Store so the identity check compares the underlying store pointer.
		{"graphBeadStore", func() beads.Store { return cr.graphBeadStore().Store }},
		// sessionsBeadStore returns the strongly-typed beads.SessionStore; unwrap its
		// embedded .Store so the identity check compares the underlying store pointer.
		{"sessionsBeadStore", func() beads.Store { return cr.sessionsBeadStore().Store }},
		// mailBeadStore returns the strongly-typed beads.MailStore; unwrap its
		// embedded .Store so the identity check compares the underlying store pointer.
		{"mailBeadStore", func() beads.Store { return cr.mailBeadStore().Store }},
		// nudgesBeadStore returns the strongly-typed beads.NudgesStore; unwrap its
		// embedded .Store so the identity check compares the underlying store pointer.
		{"nudgesBeadStore", func() beads.Store { return cr.nudgesBeadStore().Store }},
		// cityWorkStore returns the strongly-typed beads.WorkStore; unwrap its embedded
		// .Store so the identity check compares the underlying store pointer.
		{"cityWorkStore", func() beads.Store { return cr.cityWorkStore().Store }},
	}
	for _, acc := range accessors {
		if got := acc.got(); !sameStorePtr(got, city) {
			t.Errorf("CityRuntime.%s() = %p, want cityBeadStore %p", acc.name, got, city)
		}
	}
	if got := cr.ordersBeadStore("myrig").Store; !sameStorePtr(got, city) {
		t.Errorf("CityRuntime.ordersBeadStore() = %p, want cityBeadStore %p", got, city)
	}

	work := cr.workBeadStores()
	want := cr.rigBeadStores()
	if len(work) != len(want) {
		t.Fatalf("workBeadStores() len = %d, want %d", len(work), len(want))
	}
	for name, store := range want {
		// work[name] is a strongly-typed beads.WorkStore; unwrap its embedded .Store
		// so the identity check compares the underlying store pointer.
		if !sameStorePtr(work[name].Store, store) {
			t.Errorf("workBeadStores()[%q] = %p, want %p", name, work[name].Store, store)
		}
	}
}

// TestCityRuntimeClassAccessorsRouteToInfraStore mirrors the controller
// split-routing pin for the CityRuntime accessors: with standaloneCityInfraStore
// set, every coordination-class accessor returns the infra pointer while the
// work-class accessors return the city/rig pointers.
func TestCityRuntimeClassAccessorsRouteToInfraStore(t *testing.T) {
	city := beads.NewMemStore()
	infra := beads.NewMemStore()
	rig := beads.NewMemStore()
	cr := &CityRuntime{
		cityName:                 "test-city",
		standaloneCityStore:      city,
		standaloneCityInfraStore: infra,
		standaloneRigStores:      map[string]beads.Store{"myrig": rig},
	}

	accessors := []struct {
		name string
		got  func() beads.Store
	}{
		{"graphBeadStore", func() beads.Store { return cr.graphBeadStore().Store }},
		{"sessionsBeadStore", func() beads.Store { return cr.sessionsBeadStore().Store }},
		{"mailBeadStore", func() beads.Store { return cr.mailBeadStore().Store }},
		{"nudgesBeadStore", func() beads.Store { return cr.nudgesBeadStore().Store }},
		{"ordersBeadStore", func() beads.Store { return cr.ordersBeadStore("myrig").Store }},
	}
	for _, acc := range accessors {
		if got := acc.got(); !sameStorePtr(got, infra) {
			t.Errorf("CityRuntime.%s() = %p, want standaloneCityInfraStore %p", acc.name, got, infra)
		}
	}

	// Work class stays on the city/rig stores.
	if got := cr.cityWorkStore().Store; !sameStorePtr(got, city) {
		t.Errorf("CityRuntime.cityWorkStore() = %p, want cityBeadStore %p", got, city)
	}
	work := cr.workBeadStores()
	want := cr.rigBeadStores()
	if len(work) != len(want) {
		t.Fatalf("workBeadStores() len = %d, want %d", len(work), len(want))
	}
	for name, store := range want {
		if !sameStorePtr(work[name].Store, store) {
			t.Errorf("workBeadStores()[%q] = %p, want %p", name, work[name].Store, store)
		}
	}
}

// sameStorePtr reports pointer identity between two stores.
func sameStorePtr(a, b beads.Store) bool {
	ka, oka := storePointerKey(a)
	kb, okb := storePointerKey(b)
	return oka && okb && ka == kb
}
