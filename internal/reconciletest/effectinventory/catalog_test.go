package effectinventory

import (
	"reflect"
	"strings"
	"testing"
)

func TestExpandCatalogPartitionIsDeterministicAndDoesNotAlias(t *testing.T) {
	registry := validRegistry()
	template := cloneRoute(*firstRoute(&registry))
	template.LogicalOwner = FunctionRef{}
	template.Hops = nil

	classes := []catalogRouteClass{{
		ID:         "route-recovery-write",
		Definition: template,
	}}
	first := registry.Registrations[0]
	second := first
	second.Matcher.Ordinal++
	rows := []catalogSiteRow{
		{
			BoundaryID: first.BoundaryID,
			Matcher:    second.Matcher,
			Profiles:   []BuildProfileID{BuildLinuxDefault, BuildDarwinDefault},
			Classes:    []catalogRouteClassID{"route-recovery-write"},
		},
		{
			BoundaryID: first.BoundaryID,
			Matcher:    first.Matcher,
			Profiles:   []BuildProfileID{BuildLinuxDefault, BuildDarwinDefault},
			Classes:    []catalogRouteClassID{"route-recovery-write"},
		},
	}

	forward, err := expandCatalogPartition(classes, rows)
	if err != nil {
		t.Fatalf("expandCatalogPartition(forward) failed: %v", err)
	}
	reverseRows := []catalogSiteRow{rows[1], rows[0]}
	reverse, err := expandCatalogPartition(classes, reverseRows)
	if err != nil {
		t.Fatalf("expandCatalogPartition(reverse) failed: %v", err)
	}
	if !reflect.DeepEqual(forward, reverse) {
		t.Fatalf("expansion depends on row order:\nforward=%#v\nreverse=%#v", forward, reverse)
	}
	if got, want := forward[0].Cases[0].BuildProfiles, []BuildProfileID{BuildDarwinDefault, BuildLinuxDefault}; !reflect.DeepEqual(got, want) {
		t.Fatalf("profiles = %v, want canonical %v", got, want)
	}
	for _, registration := range forward {
		if got, want := registration.Cases[0].Routes[0].LogicalOwner, registration.Matcher.Enclosing; !got.equal(want) {
			t.Fatalf("logical owner = %#v, want physical enclosing %#v", got, want)
		}
	}

	compiledRegistry := registry
	compiledRegistry.Registrations = forward
	if _, err := CompileRegistry(compiledRegistry, discoveryForRegistry(compiledRegistry), validationDate()); err != nil {
		t.Fatalf("expanded catalog failed structural compilation: %v", err)
	}

	forward[0].Matcher.Enclosing.ClosurePath = append(forward[0].Matcher.Enclosing.ClosurePath, 99)
	forward[0].Cases[0].BuildProfiles[0] = BuildWindowsCompile
	forward[0].Cases[0].Routes[0].Target.Identities[0].BoundarySlot.Index = 99
	forward[0].Cases[0].Routes[0].Disposition.Gates[0] = "P9.9"
	forward[0].Cases[0].Routes[0].OwningTests[0].Name = "TestMutated"
	forward[0].Cases[0].Routes[0].Exception.RemovalTasks[0] = "P9.9"

	again, err := expandCatalogPartition(classes, rows)
	if err != nil {
		t.Fatalf("expandCatalogPartition(after mutation) failed: %v", err)
	}
	if !reflect.DeepEqual(again, reverse) {
		t.Fatalf("expanded registrations alias templates or input rows:\ngot=%#v\nwant=%#v", again, reverse)
	}
}

func TestExpandCatalogPartitionRejectsInvalidAuthorshipDeterministically(t *testing.T) {
	registry := validRegistry()
	template := cloneRoute(*firstRoute(&registry))
	template.LogicalOwner = FunctionRef{}
	template.Hops = nil
	validClass := catalogRouteClass{
		ID:         "route-recovery-write",
		Definition: template,
	}
	validRow := catalogSiteRow{
		BoundaryID: registry.Registrations[0].BoundaryID,
		Matcher:    registry.Registrations[0].Matcher,
		Profiles:   []BuildProfileID{BuildDarwinDefault},
		Classes:    []catalogRouteClassID{validClass.ID},
	}

	tests := []struct {
		name    string
		classes []catalogRouteClass
		rows    []catalogSiteRow
		want    []string
	}{
		{
			name:    "duplicate class",
			classes: []catalogRouteClass{validClass, validClass},
			rows:    []catalogSiteRow{validRow},
			want:    []string{`duplicate route class "route-recovery-write"`},
		},
		{
			name:    "unknown class",
			classes: []catalogRouteClass{validClass},
			rows: []catalogSiteRow{{
				BoundaryID: validRow.BoundaryID,
				Matcher:    validRow.Matcher,
				Profiles:   validRow.Profiles,
				Classes:    []catalogRouteClassID{"not-authored"},
			}},
			want: []string{`unknown route class "not-authored"`},
		},
		{
			name:    "duplicate physical row",
			classes: []catalogRouteClass{validClass},
			rows:    []catalogSiteRow{validRow, validRow},
			want:    []string{"duplicates physical row"},
		},
		{
			name: "prefilled owner",
			classes: []catalogRouteClass{{
				ID:         validClass.ID,
				Definition: *firstRoute(&registry),
			}},
			rows: []catalogSiteRow{validRow},
			want: []string{"must leave logical owner and hops empty"},
		},
		{
			name:    "missing profiles and classes",
			classes: []catalogRouteClass{validClass},
			rows: []catalogSiteRow{{
				BoundaryID: validRow.BoundaryID,
				Matcher:    validRow.Matcher,
			}},
			want: []string{"build profiles are required", "route classes are required"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, forwardErr := expandCatalogPartition(tt.classes, tt.rows)
			if forwardErr == nil {
				t.Fatal("expandCatalogPartition() returned nil error")
			}
			reversedClasses := append([]catalogRouteClass(nil), tt.classes...)
			reversedRows := append([]catalogSiteRow(nil), tt.rows...)
			reverseCatalogTest(reversedClasses)
			reverseCatalogTest(reversedRows)
			_, reverseErr := expandCatalogPartition(reversedClasses, reversedRows)
			if reverseErr == nil {
				t.Fatal("expandCatalogPartition(reversed) returned nil error")
			}
			if forwardErr.Error() != reverseErr.Error() {
				t.Fatalf("diagnostic depends on input order:\nforward=%v\nreverse=%v", forwardErr, reverseErr)
			}
			for _, want := range tt.want {
				if !strings.Contains(forwardErr.Error(), want) {
					t.Errorf("error = %q, want substring %q", forwardErr, want)
				}
			}
		})
	}
}

func reverseCatalogTest[T any](values []T) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}
