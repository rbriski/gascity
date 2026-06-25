package codectest

import (
	"reflect"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// beadFieldsEqual reports whether two beads are equal across every
// behaviorally-significant field, treating nil and empty maps/slices as equal
// and comparing timestamps with time.Equal (so monotonic-clock/location skew
// does not produce false negatives). It returns the first differing field name
// for a precise failure message. This is the full-bead invariant the codec
// round-trip must satisfy — metadata fidelity alone is insufficient because the
// lifecycle projections read Status, CreatedAt, Labels, and friends directly.
func beadFieldsEqual(a, b beads.Bead) (bool, string) {
	switch {
	case a.ID != b.ID:
		return false, "ID"
	case a.Title != b.Title:
		return false, "Title"
	case a.Status != b.Status:
		return false, "Status"
	case a.Type != b.Type:
		return false, "Type"
	case a.Assignee != b.Assignee:
		return false, "Assignee"
	case a.From != b.From:
		return false, "From"
	case a.ParentID != b.ParentID:
		return false, "ParentID"
	case a.Ref != b.Ref:
		return false, "Ref"
	case a.Description != b.Description:
		return false, "Description"
	case a.Ephemeral != b.Ephemeral:
		return false, "Ephemeral"
	case a.NoHistory != b.NoHistory:
		return false, "NoHistory"
	case !a.CreatedAt.Equal(b.CreatedAt):
		return false, "CreatedAt"
	case !a.UpdatedAt.Equal(b.UpdatedAt):
		return false, "UpdatedAt"
	case !intPtrEqual(a.Priority, b.Priority):
		return false, "Priority"
	case !boolPtrEqual(a.IsBlocked, b.IsBlocked):
		return false, "IsBlocked"
	case !timePtrEqual(a.DeferUntil, b.DeferUntil):
		return false, "DeferUntil"
	case !stringSliceEqual(a.Needs, b.Needs):
		return false, "Needs"
	case !stringSliceEqual(a.Labels, b.Labels):
		return false, "Labels"
	case !metadataEqual(a.Metadata, b.Metadata):
		return false, "Metadata"
	case !depsEqual(a.Dependencies, b.Dependencies):
		return false, "Dependencies"
	default:
		return true, ""
	}
}

// metadataEqual compares two metadata maps treating nil and empty as equal.
func metadataEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// projectionEqual compares two projection outputs. reflect.DeepEqual is correct
// for the typed projection structs (e.g. session lifecycle states) these return.
func projectionEqual(a, b any) bool { return reflect.DeepEqual(a, b) }

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func depsEqual(a, b []beads.Dep) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func intPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func boolPtrEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func timePtrEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
