package session

import (
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestListFromInfosMatchesListFullFromBeads is the oracle that lets WI-6 W2 delete
// Manager.ListFullFromBeads: the Info-fed typed listing must produce exactly the
// same enriched session set as the retired bead-fed listing across the corpus
// (including the type-only label-lost and label-only repairable beads the union
// feed surfaces) and the state/template filter matrix.
func TestListFromInfosMatchesListFullFromBeads(t *testing.T) {
	at := func(min int) time.Time {
		return time.Date(2026, 1, 2, 3, 4, min, 0, time.UTC)
	}
	corpus := []beads.Bead{
		{ID: "canonical", Type: BeadType, Status: "open", Labels: []string{LabelSession},
			Metadata: map[string]string{"state": "asleep", "template": "polecat", "session_name": "canonical"}, CreatedAt: at(1)},
		{ID: "type-only", Type: BeadType, Status: "open", // label lost after a crash
			Metadata: map[string]string{"state": "active", "template": "polecat", "session_name": "type-only"}, CreatedAt: at(2)},
		{ID: "label-only", Type: "", Status: "open", Labels: []string{LabelSession}, // type lost, repairable
			Metadata: map[string]string{"state": "asleep", "template": "sky", "session_name": "label-only"}, CreatedAt: at(3)},
		{ID: "non-session", Type: "task", Status: "open", Labels: []string{"work"},
			Metadata: map[string]string{"state": "active"}, CreatedAt: at(4)},
		{ID: "closed", Type: BeadType, Status: "closed", Labels: []string{LabelSession},
			Metadata: map[string]string{"state": "asleep", "template": "polecat", "session_name": "closed"}, CreatedAt: at(5)},
	}

	infos := make([]Info, 0, len(corpus))
	for _, b := range corpus {
		infos = append(infos, InfoFromPersistedBead(b))
	}

	mgr := NewManager(beads.NewMemStore(), runtime.NewFake())

	for _, sf := range []string{"", "asleep", "active", "all", "closed", "active,asleep"} {
		for _, tf := range []string{"", "polecat", "sky"} {
			got := mgr.ListFromInfos(infos, sf, tf)
			want := mgr.ListFullFromBeads(corpus, sf, tf).Sessions
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ListFromInfos(state=%q,template=%q) diverged from ListFullFromBeads:\n got = %+v\nwant = %+v", sf, tf, got, want)
			}
		}
	}
}
