package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// WI-5 W3 per-parameter-split oracles. These pin the Info forms of the
// mixed work/session helpers (spec §7): the SESSION parameter reads typed
// session.Info while the WORK bead slice / request stay raw. Each Info form
// must be byte-identical to reading the raw session bead.

// oracleSessionBeadShapes returns representative session beads covering the
// field regions the W3 session-side splits read: bare, pool-managed with a
// session_name, a named session with a configured identity, and one carrying a
// work_dir. Byte-identity must hold across every shape.
func oracleSessionBeadShapes() []beads.Bead {
	mk := func(id string, m map[string]string) beads.Bead {
		return beads.Bead{ID: id, Type: session.BeadType, Status: "open", Labels: []string{session.LabelSession}, Metadata: m}
	}
	return []beads.Bead{
		mk("ga-bare", map[string]string{"template": "worker"}),
		mk("ga-pool", map[string]string{
			"template": "worker", "session_name": "worker-ga-pool",
			"pool_managed": "true", "pool_slot": "1", "work_dir": "/w/pool",
		}),
		mk("ga-named", map[string]string{
			"template": "mayor", "configured_named_session": "true",
			"configured_named_identity": "mayor", "alias": "mayor",
			"session_name": "mayor", "alias_history": "mayor,boss",
		}),
		mk("ga-named-fallback", map[string]string{
			"template": "mayor", "configured_named_session": "true",
			"session_name": "mayor",
		}),
		mk("ga-noname", map[string]string{"template": "worker", "work_dir": "/w/x"}),
	}
}

// TestSessionBeadHasAssignedWorkInfoMatchesRaw proves the session-side split of
// sessionBeadHasAssignedWork: for a fixed set of work beads, the Info form and
// the raw form agree across every session-bead shape.
func TestSessionBeadHasAssignedWorkInfoMatchesRaw(t *testing.T) {
	work := []beads.Bead{
		{ID: "wb-open-id", Status: "open", Assignee: "ga-pool"},
		{ID: "wb-name", Status: "in_progress", Assignee: "worker-ga-pool"},
		{ID: "wb-ident", Status: "open", Assignee: "mayor"},
		{ID: "wb-closed", Status: "closed", Assignee: "ga-pool"},
		{ID: "wb-blank", Status: "open", Assignee: ""},
		{ID: "wb-unmatched", Status: "in_progress", Assignee: "nobody"},
	}
	for _, sb := range oracleSessionBeadShapes() {
		info := session.InfoFromPersistedBead(sb)
		if got, want := sessionBeadHasAssignedWorkInfo(work, info), sessionBeadHasAssignedWork(work, sb); got != want {
			t.Errorf("sessionBeadHasAssignedWork(%s): info=%v raw=%v", sb.ID, got, want)
		}
	}
	// Empty work slice must be false on both forms.
	for _, sb := range oracleSessionBeadShapes() {
		info := session.InfoFromPersistedBead(sb)
		if got, want := sessionBeadHasAssignedWorkInfo(nil, info), sessionBeadHasAssignedWork(nil, sb); got != want {
			t.Errorf("sessionBeadHasAssignedWork(nil, %s): info=%v raw=%v", sb.ID, got, want)
		}
	}
}

// TestSessionCoreConfigForHashInfoMatchesRaw pins the config-drift fingerprint
// helper: the Info form must equal the raw wrapper for every session-bead shape.
// The wrapper feeds the drift key, so any divergence would silently repartition
// which sessions the reconciler treats as drifted (W2 deferred this for lack of
// an oracle row; here it is pinned hard).
func TestSessionCoreConfigForHashInfoMatchesRaw(t *testing.T) {
	tps := []TemplateParams{
		{},
		{TemplateName: "worker"},
		{TemplateName: "worker", Command: "claude --model x"},
	}
	for _, tp := range tps {
		for _, sb := range oracleSessionBeadShapes() {
			info := session.InfoFromPersistedBead(sb)
			got := sessionCoreConfigForHashInfo(tp, info)
			want := sessionCoreConfigForHash(tp, sb)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("sessionCoreConfigForHash(%s): info=%+v raw=%+v", sb.ID, got, want)
			}
		}
	}
}

// rawPoolTriggerBindingPatchRef is an independent reimplementation of the
// trigger/pack/workspace/work-dir key-diff that bindPoolSessionTriggerBead
// computed inline against sessionBead.Metadata, kept in the test as the ground
// truth computePoolTriggerBindingPatch must match. It reads the RAW bead
// metadata directly so the oracle proves the Info projection is byte-identical.
func rawPoolTriggerBindingPatchRef(sb beads.Bead, request SessionRequest, workDir string) session.MetadataPatch {
	workBeadID := strings.TrimSpace(request.WorkBeadID)
	metadata := session.MetadataPatch{}
	if workBeadID == "" {
		if strings.TrimSpace(sb.Metadata[beadmeta.TriggerBeadIDMetadataKey]) != "" {
			metadata[beadmeta.TriggerBeadIDMetadataKey] = ""
		}
		if strings.TrimSpace(sb.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey]) != "" {
			metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = ""
		}
		if strings.TrimSpace(sb.Metadata[beadmeta.BrainParentSIDMetadataKey]) != "" {
			metadata[beadmeta.BrainParentSIDMetadataKey] = ""
		}
		return metadata
	}
	oldWorkBeadID := strings.TrimSpace(sb.Metadata[beadmeta.TriggerBeadIDMetadataKey])
	if oldWorkBeadID != workBeadID {
		metadata[beadmeta.TriggerBeadIDMetadataKey] = workBeadID
		newParentSID := strings.TrimSpace(request.BrainParentSID)
		if strings.TrimSpace(sb.Metadata[beadmeta.BrainParentSIDMetadataKey]) != newParentSID {
			metadata[beadmeta.BrainParentSIDMetadataKey] = newParentSID
		}
	}
	workStoreRef := strings.TrimSpace(request.WorkStoreRef)
	if workStoreRef != "" && strings.TrimSpace(sb.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey]) != workStoreRef {
		metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = workStoreRef
	} else if workStoreRef == "" && oldWorkBeadID != workBeadID && strings.TrimSpace(sb.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey]) != "" {
		metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = ""
	}
	if pack := strings.TrimSpace(request.WorkPack); strings.TrimSpace(sb.Metadata[beadmeta.PackMetadataKey]) != pack {
		metadata[beadmeta.PackMetadataKey] = pack
	}
	if workspace := packWorkspaceSlug(request); strings.TrimSpace(sb.Metadata[beadmeta.PackWorkspaceMetadataKey]) != workspace {
		metadata[beadmeta.PackWorkspaceMetadataKey] = workspace
	}
	if workDir != "" {
		if strings.TrimSpace(sb.Metadata[beadmeta.WorkDirMetadataKey]) != workDir {
			metadata[beadmeta.WorkDirMetadataKey] = workDir
		}
		if strings.TrimSpace(sb.Metadata[beadmeta.LegacyWorkDirMetadataKey]) != workDir {
			metadata[beadmeta.LegacyWorkDirMetadataKey] = workDir
		}
	}
	return metadata
}

// TestComputePoolTriggerBindingPatchMatchesRaw pins the extracted pure diff
// against the independent raw reference across the clear, reassign, store-ref,
// pack, workspace, and work-dir request shapes, on both a bare session bead and
// one already carrying a full trigger cluster.
func TestComputePoolTriggerBindingPatchMatchesRaw(t *testing.T) {
	bases := map[string]beads.Bead{
		"bare": {ID: "s-bare", Type: session.BeadType, Status: "open", Labels: []string{session.LabelSession}, Metadata: map[string]string{}},
		"full": {ID: "s-full", Type: session.BeadType, Status: "open", Labels: []string{session.LabelSession}, Metadata: map[string]string{
			beadmeta.TriggerBeadIDMetadataKey:       "wb-old",
			beadmeta.TriggerBeadStoreRefMetadataKey: "rig-old",
			beadmeta.BrainParentSIDMetadataKey:      "brain-old",
			beadmeta.PackMetadataKey:                "pack-old",
			beadmeta.PackWorkspaceMetadataKey:       "ws-old",
			beadmeta.WorkDirMetadataKey:             "/gc/old",
			beadmeta.LegacyWorkDirMetadataKey:       "/old",
		}},
	}
	requests := map[string]SessionRequest{
		"clear":             {WorkBeadID: ""},
		"reassign-same":     {WorkBeadID: "wb-old"},
		"reassign-diff":     {WorkBeadID: "wb-new", BrainParentSID: "brain-new"},
		"reassign-noparent": {WorkBeadID: "wb-new"},
		"store-ref":         {WorkBeadID: "wb-new", WorkStoreRef: "rig-new"},
		"pack":              {WorkBeadID: "wb-new", WorkPack: "pack-new"},
		"workspace":         {WorkBeadID: "wb-new", WorkPack: "pack-new", WorkWorkspace: "ws-new"},
	}
	workDirs := []string{"", "/gc/old", "/gc/new"}
	for bn, sb := range bases {
		info := session.InfoFromPersistedBead(sb)
		for rn, req := range requests {
			for _, wd := range workDirs {
				got := computePoolTriggerBindingPatch(info, req, wd)
				want := rawPoolTriggerBindingPatchRef(sb, req, wd)
				if !reflect.DeepEqual(map[string]string(got), map[string]string(want)) {
					t.Errorf("base=%s req=%s workDir=%q: got=%v want=%v", bn, rn, wd, got, want)
				}
			}
		}
	}
}
