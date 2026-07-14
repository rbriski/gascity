package effectinventory

import (
	"fmt"
	"sort"
	"strings"
)

type catalogRouteClassID string

// catalogRouteClass is one named, reusable semantic classification. Definition
// deliberately omits its logical owner and hops: the compact catalog currently
// supports only truthful leaf ownership, filled from each physical matcher.
type catalogRouteClass struct {
	ID         catalogRouteClassID
	Definition Route
}

// catalogSiteRow explicitly selects the route classes for one physical site.
// Profiles and matcher are authored evidence, not derived from discovery.
type catalogSiteRow struct {
	BoundaryID string
	Matcher    OperationSite
	Profiles   []BuildProfileID
	Classes    []catalogRouteClassID
}

func expandCatalogPartition(classes []catalogRouteClass, rows []catalogSiteRow) ([]SiteRegistration, error) {
	var problems []string
	classByID := make(map[catalogRouteClassID]catalogRouteClass, len(classes))
	for _, class := range classes {
		scope := fmt.Sprintf("route class %q", class.ID)
		if strings.TrimSpace(string(class.ID)) == "" {
			problems = append(problems, scope+": id is required")
		}
		if _, exists := classByID[class.ID]; exists {
			problems = append(problems, fmt.Sprintf("duplicate route class %q", class.ID))
		} else {
			classByID[class.ID] = class
		}
		if !class.Definition.LogicalOwner.equal(FunctionRef{}) || len(class.Definition.Hops) != 0 {
			problems = append(problems, scope+": physical-enclosing template must leave logical owner and hops empty")
		}
	}

	physicalRows := make(map[string]bool, len(rows))
	for index, row := range rows {
		scope := fmt.Sprintf("site row %d (%s)", index, describePhysicalSite(row.BoundaryID, row.Matcher))
		physicalKey := registrationPhysicalKey(row.BoundaryID, row.Matcher)
		if physicalRows[physicalKey] {
			problems = append(problems, scope+": duplicates physical row")
		}
		physicalRows[physicalKey] = true
		if len(row.Profiles) == 0 {
			problems = append(problems, scope+": build profiles are required")
		}
		profileSeen := make(map[BuildProfileID]bool, len(row.Profiles))
		for _, profile := range row.Profiles {
			if profileSeen[profile] {
				problems = append(problems, fmt.Sprintf("%s: duplicate build profile %q", scope, profile))
			}
			profileSeen[profile] = true
		}
		if len(row.Classes) == 0 {
			problems = append(problems, scope+": route classes are required")
		}
		classSeen := make(map[catalogRouteClassID]bool, len(row.Classes))
		for _, classID := range row.Classes {
			if classSeen[classID] {
				problems = append(problems, fmt.Sprintf("%s: duplicate route class %q", scope, classID))
			}
			classSeen[classID] = true
			if _, exists := classByID[classID]; !exists {
				problems = append(problems, fmt.Sprintf("%s: unknown route class %q", scope, classID))
			}
		}
	}

	if len(problems) != 0 {
		sort.Strings(problems)
		problems = compactStrings(problems)
		return nil, fmt.Errorf("expand catalog partition:\n- %s", strings.Join(problems, "\n- "))
	}

	registrations := make([]SiteRegistration, 0, len(rows))
	for _, row := range rows {
		profiles := append([]BuildProfileID(nil), row.Profiles...)
		sort.Slice(profiles, func(i, j int) bool { return profiles[i] < profiles[j] })
		classIDs := append([]catalogRouteClassID(nil), row.Classes...)
		sort.Slice(classIDs, func(i, j int) bool { return classIDs[i] < classIDs[j] })
		routes := make([]Route, 0, len(classIDs))
		for _, classID := range classIDs {
			route := cloneRoute(classByID[classID].Definition)
			route.LogicalOwner = cloneOperationSite(row.Matcher).Enclosing
			routes = append(routes, route)
		}
		registrations = append(registrations, SiteRegistration{
			BoundaryID: row.BoundaryID,
			Matcher:    cloneOperationSite(row.Matcher),
			Cases: []ProfileCase{{
				BuildProfiles: profiles,
				Routes:        routes,
			}},
		})
	}
	sort.Slice(registrations, func(i, j int) bool {
		left := registrationPhysicalKey(registrations[i].BoundaryID, registrations[i].Matcher)
		right := registrationPhysicalKey(registrations[j].BoundaryID, registrations[j].Matcher)
		return left < right
	})
	return registrations, nil
}
