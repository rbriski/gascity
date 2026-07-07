package rig

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestValidateDepsRequiresInfra(t *testing.T) {
	full := Deps{FS: fsys.OSFS{}, CityPath: "/city", Cfg: &config.City{}}
	if err := validateDeps(full); err != nil {
		t.Fatalf("full deps should validate, got: %v", err)
	}

	cases := map[string]Deps{
		"missing FS":       {CityPath: "/city", Cfg: &config.City{}},
		"missing CityPath": {FS: fsys.OSFS{}, Cfg: &config.City{}},
		"missing Cfg":      {FS: fsys.OSFS{}, CityPath: "/city"},
	}
	for name, d := range cases {
		if err := validateDeps(d); err == nil {
			t.Errorf("%s: expected a validation error, got nil", name)
		}
	}
}

func TestProvisionValidatesBeforeRunning(t *testing.T) {
	// An incomplete Deps must fail at validation, never reach the (stubbed) core.
	_, _, err := Provision(Deps{}, ProvisionRequest{Name: "x", Path: "/x"})
	if err == nil {
		t.Fatal("Provision with empty deps should error at validation")
	}
	if errors.Is(err, ErrNotImplemented) {
		t.Fatal("Provision should fail deps validation before reaching the not-implemented core")
	}
}

func TestProvisionValidatesRequest(t *testing.T) {
	deps := Deps{FS: fsys.OSFS{}, CityPath: "/city", Cfg: &config.City{}}
	for name, req := range map[string]ProvisionRequest{
		"missing name": {Path: "/x"},
		"missing path": {Name: "x"},
	} {
		_, _, err := Provision(deps, req)
		if err == nil {
			t.Errorf("%s: expected a request-validation error, got nil", name)
			continue
		}
		if errors.Is(err, ErrNotImplemented) {
			t.Errorf("%s: should fail request validation before the not-implemented core", name)
		}
	}
}

func TestProvisionReachesCoreWhenValid(t *testing.T) {
	deps := Deps{FS: fsys.OSFS{}, CityPath: "/city", Cfg: &config.City{}}
	_, _, err := Provision(deps, ProvisionRequest{Name: "x", Path: "/x"})
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("valid deps+request should reach the not-implemented core, got: %v", err)
	}
}
