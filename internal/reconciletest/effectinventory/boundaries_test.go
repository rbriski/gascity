package effectinventory

import (
	"context"
	"strings"
	"testing"
)

func TestCanonicalBoundariesContainAuditedTypedVocabulary(t *testing.T) {
	boundaries := CanonicalBoundaries()
	if len(boundaries) == 0 {
		t.Fatal("CanonicalBoundaries() returned no definitions")
	}
	var problems []string
	validateBoundaries(boundaries, &problems)
	if len(problems) != 0 {
		t.Fatalf("canonical boundary validation failed:\n- %s", strings.Join(problems, "\n- "))
	}

	byObject := make(map[string]BoundaryDefinition, len(boundaries))
	for _, boundary := range boundaries {
		key := boundary.Object.key()
		if previous, exists := byObject[key]; exists {
			t.Fatalf("boundary object %s is duplicated by %q and %q", key, previous.ID, boundary.ID)
		}
		byObject[key] = boundary
	}

	for _, want := range []struct {
		object ObjectRef
		kind   EffectKind
		match  ObjectMatchKind
	}{
		{ObjectRef{Package: "github.com/gastownhall/gascity/internal/beads", Receiver: "Writer", Name: "Update"}, KindStoreMutation, ObjectMatchInterfaceImplementors},
		{ObjectRef{Package: "github.com/gastownhall/gascity/internal/beads", Receiver: "ConditionalAssignmentReleaser", Name: "ReleaseIfCurrent"}, KindStoreMutation, ObjectMatchInterfaceImplementors},
		{ObjectRef{Package: "github.com/gastownhall/gascity/internal/beads", Receiver: "BdStore", Name: "Claim"}, KindStoreMutation, ObjectMatchExact},
		{ObjectRef{Package: "github.com/gastownhall/gascity/internal/runtime", Receiver: "Provider", Name: "Attach"}, KindProviderMutation, ObjectMatchInterfaceImplementors},
		{ObjectRef{Package: "github.com/gastownhall/gascity/internal/runtime", Receiver: "Runtime", Name: "Provision"}, KindProviderMutation, ObjectMatchInterfaceImplementors},
		{ObjectRef{Package: "github.com/gastownhall/gascity/internal/runtime", Receiver: "Transport", Name: "Launch"}, KindProviderMutation, ObjectMatchInterfaceImplementors},
		{ObjectRef{Package: "github.com/gastownhall/gascity/internal/runtime", Receiver: "Attachment", Name: "Close"}, KindProviderMutation, ObjectMatchInterfaceImplementors},
		{ObjectRef{Package: "github.com/gastownhall/gascity/internal/events", Receiver: "Recorder", Name: "Record"}, KindEventEmission, ObjectMatchInterfaceImplementors},
		{ObjectRef{Package: "github.com/gastownhall/gascity/internal/pidutil", Name: "Signal"}, KindProcessMutation, ObjectMatchExact},
		{ObjectRef{Package: "github.com/gastownhall/gascity/internal/processgroup", Name: "TerminateCommand"}, KindProcessMutation, ObjectMatchExact},
	} {
		got, ok := byObject[want.object.key()]
		if !ok {
			t.Errorf("canonical boundary for %s is missing", want.object.key())
			continue
		}
		if got.Kind != want.kind || got.Match != want.match {
			t.Errorf("canonical boundary for %s = kind %q match %q, want kind %q match %q", want.object.key(), got.Kind, got.Match, want.kind, want.match)
		}
	}

	forbidden := []ObjectRef{
		{Package: "github.com/gastownhall/gascity/internal/runtime", Receiver: "Provider", Name: "SetMeta"},
		{Package: "github.com/gastownhall/gascity/internal/runtime", Receiver: "Provider", Name: "RemoveMeta"},
		{Package: "syscall", Name: "Kill"},
	}
	for _, method := range []string{"Create", "Update", "Close", "Reopen", "CloseAll", "SetMetadata", "SetMetadataBatch", "Delete", "DepAdd", "DepRemove"} {
		forbidden = append(forbidden, ObjectRef{Package: "github.com/gastownhall/gascity/internal/beads", Receiver: "Store", Name: method})
	}
	for _, forbidden := range forbidden {
		if boundary, exists := byObject[forbidden.key()]; exists {
			t.Errorf("overlapping or platform-incomplete boundary %s is registered as %q", forbidden.key(), boundary.ID)
		}
	}
}

func TestCanonicalBoundariesReturnIndependentSlices(t *testing.T) {
	first := CanonicalBoundaries()
	first[0].ID = "mutated"
	second := CanonicalBoundaries()
	if second[0].ID == "mutated" {
		t.Fatal("CanonicalBoundaries() retained caller mutation")
	}
}

func TestCanonicalBoundariesResolveInEveryAnalysisProfile(t *testing.T) {
	config := fixtureAnalysisConfig(t, nil)
	patterns, err := canonicalSourcePatterns(canonicalAnalysisRoots())
	if err != nil {
		t.Fatalf("canonicalSourcePatterns() error: %v", err)
	}
	config.Patterns = patterns

	for _, profile := range canonicalAnalysisProfiles() {
		t.Run(string(profile.ID), func(t *testing.T) {
			analysis, err := loadAnalysis(context.Background(), config, profile)
			if err != nil {
				t.Fatalf("loadAnalysis() error: %v", err)
			}
			if _, err := resolveBoundaries(analysis.packages, CanonicalBoundaries()); err != nil {
				t.Fatalf("resolveBoundaries() error: %v", err)
			}
		})
	}
}
