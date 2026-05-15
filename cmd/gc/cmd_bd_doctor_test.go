package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"strings"
	"testing"

	gcapi "github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
)

func TestBdDoctor_RejectsWithoutOperation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := doBdDoctor("", "", nil, strings.NewReader(""), &stdout, &stderr)
	if exit != 2 {
		t.Fatalf("exit = %d, want 2", exit)
	}
	if !strings.Contains(stderr.String(), "pass --reseed-identity") {
		t.Fatalf("stderr = %q, want reseed guidance", stderr.String())
	}
}

func TestBdDoctor_ReseedHappyPath_Interactive(t *testing.T) {
	state := newBdDoctorTestState(t)
	state.interactive = true
	var stdout, stderr bytes.Buffer

	exit := doBdDoctor("", "", []string{"--reseed-identity"}, strings.NewReader("yes\n"), &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	if !state.upserted {
		t.Fatal("upsert not called")
	}
	state.assertEvent(t, "old-l3", "canonical")
}

func TestBdDoctor_ReseedHappyPath_AssumeYes(t *testing.T) {
	state := newBdDoctorTestState(t)
	state.interactive = false
	var stdout, stderr bytes.Buffer

	exit := doBdDoctor("", "", []string{"--reseed-identity", "--yes"}, strings.NewReader(""), &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	if !state.upserted {
		t.Fatal("upsert not called")
	}
}

func TestBdDoctor_ReseedRefusedOnEmptyConfirm(t *testing.T) {
	state := newBdDoctorTestState(t)
	state.interactive = true
	var stdout, stderr bytes.Buffer

	exit := doBdDoctor("", "", []string{"--reseed-identity"}, strings.NewReader("\n"), &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	if state.upserted {
		t.Fatal("upsert called after refusal")
	}
	if !strings.Contains(stdout.String(), "refused") {
		t.Fatalf("stdout = %q, want refused message", stdout.String())
	}
}

func TestBdDoctor_ReseedRefusedOnNoInput(t *testing.T) {
	state := newBdDoctorTestState(t)
	state.interactive = false
	var stdout, stderr bytes.Buffer

	exit := doBdDoctor("", "", []string{"--reseed-identity", "--no-input"}, strings.NewReader(""), &stdout, &stderr)
	if exit != 1 {
		t.Fatalf("exit = %d, want 1; stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	if state.upserted {
		t.Fatal("upsert called after no-input refusal")
	}
	if !strings.Contains(stderr.String(), "rerun with `--yes`") {
		t.Fatalf("stderr = %q, want --yes guidance", stderr.String())
	}
}

func TestBdDoctor_ReseedRefusedOnLowercaseY(t *testing.T) {
	state := newBdDoctorTestState(t)
	state.interactive = true
	var stdout, stderr bytes.Buffer

	exit := doBdDoctor("", "", []string{"--reseed-identity"}, strings.NewReader("y\n"), &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	if state.upserted {
		t.Fatal("upsert called after lowercase y")
	}
}

func TestBdDoctor_ReseedRefusedWhenL1Absent(t *testing.T) {
	state := newBdDoctorTestState(t)
	state.l1OK = false
	var stdout, stderr bytes.Buffer

	exit := doBdDoctor("", "", []string{"--reseed-identity", "--yes"}, strings.NewReader(""), &stdout, &stderr)
	if exit != 1 {
		t.Fatalf("exit = %d, want 1; stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "identity.toml is absent") {
		t.Fatalf("stderr = %q, want L1 absent message", stderr.String())
	}
}

func TestBdDoctor_ReseedRefusedWhenDoltDown(t *testing.T) {
	state := newBdDoctorTestState(t)
	state.doltOK = false
	var stdout, stderr bytes.Buffer

	exit := doBdDoctor("", "", []string{"--reseed-identity", "--yes"}, strings.NewReader(""), &stdout, &stderr)
	if exit != 1 {
		t.Fatalf("exit = %d, want 1; stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "dolt server unavailable") {
		t.Fatalf("stderr = %q, want dolt down message", stderr.String())
	}
}

func TestBdDoctor_ReseedEmitsEventOnSuccess(t *testing.T) {
	state := newBdDoctorTestState(t)
	var stdout, stderr bytes.Buffer

	exit := doBdDoctor("", "", []string{"--reseed-identity", "--yes"}, strings.NewReader(""), &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	state.assertEvent(t, "old-l3", "canonical")
}

func TestBdDoctor_ReseedScopeResolution(t *testing.T) {
	state := newBdDoctorTestState(t)
	var stdout, stderr bytes.Buffer

	exit := doBdDoctor("demo-city", "demo-rig", []string{"--reseed-identity", "--yes", "ga-123"}, strings.NewReader(""), &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	if state.cityName != "demo-city" {
		t.Fatalf("cityName = %q, want demo-city", state.cityName)
	}
	if state.rigName != "demo-rig" {
		t.Fatalf("rigName = %q, want demo-rig", state.rigName)
	}
	if len(state.tail) != 1 || state.tail[0] != "ga-123" {
		t.Fatalf("tail = %#v, want ga-123", state.tail)
	}
}

func TestBdDoctor_PassthroughForUnknownSub(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := doBdDoctor("", "", []string{"blah"}, strings.NewReader(""), &stdout, &stderr)
	if exit != 2 {
		t.Fatalf("exit = %d, want 2", exit)
	}
}

type bdDoctorTestState struct {
	cityRoot    string
	scopeRoot   string
	cityName    string
	rigName     string
	tail        []string
	l1ID        string
	l1OK        bool
	oldID       string
	doltOK      bool
	upserted    bool
	interactive bool
	events      []gcapi.ProjectIdentityStampedPayload
}

func newBdDoctorTestState(t *testing.T) *bdDoctorTestState {
	t.Helper()
	state := &bdDoctorTestState{
		cityRoot:  t.TempDir(),
		l1ID:      "canonical",
		l1OK:      true,
		oldID:     "old-l3",
		doltOK:    true,
		scopeRoot: filepath.Join(t.TempDir(), "rigs", "demo"),
	}
	t.Cleanup(installBdDoctorTestHooks(state))
	return state
}

func installBdDoctorTestHooks(state *bdDoctorTestState) func() {
	oldResolveCity := bdDoctorResolveCity
	oldLoadCityConfig := bdDoctorLoadCityConfig
	oldResolveTarget := bdDoctorResolveScopeTarget
	oldReadL1 := bdDoctorReadProjectIdentity
	oldDial := bdDoctorDialDoltForScope
	oldReadL3 := bdDoctorReadDatabaseProjectID
	oldUpsert := bdDoctorUpsertDatabaseProjectIDForce
	oldRecord := bdDoctorRecordProjectIdentityStamped
	oldInteractive := bdDoctorIsInteractive

	bdDoctorResolveCity = func(name string) (string, error) {
		state.cityName = name
		return state.cityRoot, nil
	}
	bdDoctorLoadCityConfig = func(string, io.Writer) (*config.City, error) {
		return &config.City{}, nil
	}
	bdDoctorResolveScopeTarget = func(_ *config.City, _ string, rigName string, tail []string) (execStoreTarget, error) {
		state.rigName = rigName
		state.tail = append([]string(nil), tail...)
		return execStoreTarget{ScopeRoot: state.scopeRoot, ScopeKind: "rig", RigName: "demo"}, nil
	}
	bdDoctorReadProjectIdentity = func(_ string) (string, bool, error) {
		return state.l1ID, state.l1OK, nil
	}
	bdDoctorDialDoltForScope = func(string, string) (*sql.DB, bool, error) {
		return nil, state.doltOK, nil
	}
	bdDoctorReadDatabaseProjectID = func(context.Context, *sql.DB) (string, bool, error) {
		return state.oldID, strings.TrimSpace(state.oldID) != "", nil
	}
	bdDoctorUpsertDatabaseProjectIDForce = func(context.Context, *sql.DB, string) (int64, error) {
		state.upserted = true
		return 1, nil
	}
	bdDoctorRecordProjectIdentityStamped = func(_ io.Writer, _ string, _ string, oldID, newID string) {
		state.events = append(state.events, gcapi.ProjectIdentityStampedPayload{
			ScopeRoot: "rigs/demo",
			Source:    "cache_repair",
			Layer:     "L3",
			OldID:     oldID,
			NewID:     newID,
		})
	}
	bdDoctorIsInteractive = func(io.Reader) bool {
		return state.interactive
	}

	return func() {
		bdDoctorResolveCity = oldResolveCity
		bdDoctorLoadCityConfig = oldLoadCityConfig
		bdDoctorResolveScopeTarget = oldResolveTarget
		bdDoctorReadProjectIdentity = oldReadL1
		bdDoctorDialDoltForScope = oldDial
		bdDoctorReadDatabaseProjectID = oldReadL3
		bdDoctorUpsertDatabaseProjectIDForce = oldUpsert
		bdDoctorRecordProjectIdentityStamped = oldRecord
		bdDoctorIsInteractive = oldInteractive
	}
}

func (s *bdDoctorTestState) assertEvent(t *testing.T, oldID, newID string) {
	t.Helper()
	if len(s.events) != 1 {
		t.Fatalf("events = %+v, want one", s.events)
	}
	got := s.events[0]
	if got.Source != "cache_repair" || got.Layer != "L3" || got.OldID != oldID || got.NewID != newID {
		t.Fatalf("event = %+v, want cache_repair L3 old=%q new=%q", got, oldID, newID)
	}
}
