package docsync

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // Git blob identity uses SHA-1; security uses SHA-256 below.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const reconcilerCandidateManifestPath = "engdocs/plans/reconciler-redesign/PRE_G0_CANDIDATE_MANIFEST.json"

type reconcilerCandidateManifest struct {
	SchemaVersion     int                          `json:"schema_version"`
	Kind              string                       `json:"kind"`
	Status            string                       `json:"status"`
	HashAlgorithm     string                       `json:"hash_algorithm"`
	Canonicalization  string                       `json:"canonicalization"`
	ExecutionBase     reconcilerExecutionBase      `json:"execution_base"`
	Contract          reconcilerCandidateContract  `json:"contract"`
	CurrentFiles      []reconcilerCandidateFile    `json:"current_files"`
	Sections          []reconcilerCandidateSection `json:"sections"`
	GlobalConstraints []string                     `json:"global_constraints"`
	Slices            []reconcilerCandidateSlice   `json:"slices"`
	Review            reconcilerCandidateReview    `json:"review"`
	Enforcement       reconcilerEnforcement        `json:"enforcement"`
	NonClaims         []string                     `json:"nonclaims"`
	CandidateDigest   string                       `json:"candidate_digest,omitempty"`
}

type reconcilerExecutionBase struct {
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
	Tree       string `json:"tree"`
}

type reconcilerCandidateContract struct {
	Commit string                    `json:"commit"`
	Files  []reconcilerCandidateFile `json:"files"`
}

type reconcilerCandidateFile struct {
	Path    string `json:"path"`
	BlobOID string `json:"blob_oid"`
	SHA256  string `json:"sha256"`
}

type reconcilerCandidateSection struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Selector string `json:"selector"`
	SHA256   string `json:"sha256"`
}

type reconcilerCandidateSlice struct {
	Task            string                    `json:"task"`
	Bead            string                    `json:"bead"`
	TaskHash        string                    `json:"task_hash"`
	RowHashes       []reconcilerCandidateHash `json:"row_hashes"`
	InvariantHashes []reconcilerCandidateHash `json:"invariant_hashes"`
	NonClaims       []string                  `json:"nonclaims"`
}

type reconcilerCandidateHash struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
}

type reconcilerCandidateReview struct {
	Window50EvidenceSHA256 string `json:"window_50_evidence_sha256"`
	Window50Method         string `json:"window_50_method"`
	PostIntegration        struct {
		Path                   string `json:"path"`
		SHA256                 string `json:"sha256"`
		Reviewer               string `json:"reviewer"`
		ReviewedContractCommit string `json:"reviewed_contract_commit"`
		PlanBlobOID            string `json:"plan_blob_oid"`
		MatrixBlobOID          string `json:"matrix_blob_oid"`
		Verdict                string `json:"verdict"`
	} `json:"post_integration"`
	ImplementationAuthorization struct {
		Identity               string `json:"identity"`
		Date                   string `json:"date"`
		Evidence               string `json:"evidence"`
		ExactCandidateApproval bool   `json:"exact_candidate_approval"`
		Signature              string `json:"cryptographic_signature"`
	} `json:"implementation_authorization"`
}

type reconcilerEnforcement struct {
	CodeOwner                    string `json:"code_owner"`
	ObservedMainBranchProtection struct {
		CheckedAt                string `json:"checked_at"`
		RequiredCodeOwnerReviews bool   `json:"required_code_owner_reviews"`
		EnforceAdmins            bool   `json:"enforce_admins"`
	} `json:"observed_main_branch_protection"`
	MergePrecondition string `json:"merge_precondition"`
}

type candidateSectionSpec struct {
	Path, Kind, Selector string
}

func TestReconcilerPreG0CandidateManifest(t *testing.T) {
	root := repoRoot()
	manifestBytes, err := os.ReadFile(filepath.Join(root, reconcilerCandidateManifestPath))
	if err != nil {
		t.Fatalf("reading %s: %v", reconcilerCandidateManifestPath, err)
	}
	if err := rejectDuplicateJSONKeys(manifestBytes); err != nil {
		t.Fatalf("validating unique JSON keys in %s: %v", reconcilerCandidateManifestPath, err)
	}

	var manifest reconcilerCandidateManifest
	decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("decoding %s: %v", reconcilerCandidateManifestPath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("%s contains trailing JSON or invalid data: %v", reconcilerCandidateManifestPath, err)
	}

	if manifest.SchemaVersion != 1 || manifest.Kind != "reconciler-pre-g0-candidate" || manifest.Status != "development-only" {
		t.Fatalf("unexpected manifest identity: schema=%d kind=%q status=%q", manifest.SchemaVersion, manifest.Kind, manifest.Status)
	}
	if manifest.HashAlgorithm != "sha256" || manifest.Canonicalization != "go-encoding-json-v1-candidate-digest-omitted" {
		t.Fatalf("unexpected hash contract: algorithm=%q canonicalization=%q", manifest.HashAlgorithm, manifest.Canonicalization)
	}
	if manifest.ExecutionBase.Repository != "gastownhall/gascity" {
		t.Fatalf("execution repository = %q", manifest.ExecutionBase.Repository)
	}
	for name, value := range map[string]string{
		"execution commit": manifest.ExecutionBase.Commit,
		"execution tree":   manifest.ExecutionBase.Tree,
		"contract commit":  manifest.Contract.Commit,
	} {
		if !isLowerHex(value, 40) {
			t.Errorf("%s %q is not a full SHA-1 object ID", name, value)
		}
	}
	for name, object := range map[string]string{
		"execution commit": manifest.ExecutionBase.Commit,
		"contract commit":  manifest.Contract.Commit,
	} {
		if got := gitOutput(t, root, "rev-parse", object+"^{commit}"); got != object {
			t.Errorf("%s resolves to %s, want exact commit object %s", name, got, object)
		}
	}
	if got := gitOutput(t, root, "rev-parse", manifest.ExecutionBase.Commit+"^{tree}"); got != manifest.ExecutionBase.Tree {
		t.Errorf("execution tree = %s, git resolves %s", manifest.ExecutionBase.Tree, got)
	}
	if got := gitOutput(t, root, "rev-parse", manifest.Contract.Commit+"^"); got != manifest.ExecutionBase.Commit {
		t.Errorf("contract commit parent = %s, want execution base %s", got, manifest.ExecutionBase.Commit)
	}

	wantContractFiles := []string{
		".github/CODEOWNERS",
		"engdocs/plans/reconciler-redesign/ACCEPTANCE_MATRIX.md",
		"engdocs/plans/reconciler-redesign/EXTERNAL-REVIEW-fable-10axis.md",
		"engdocs/plans/reconciler-redesign/IMPLEMENTATION_PLAN.md",
		"internal/session/REQUIREMENTS.md",
	}
	contractFiles := exactFileSet(t, "contract", manifest.Contract.Files, wantContractFiles)
	contractData := make(map[string][]byte, len(wantContractFiles))
	for _, path := range wantContractFiles {
		entry := contractFiles[path]
		data := gitFileAtCommit(t, root, manifest.Contract.Commit, path)
		assertCandidateFileHash(t, "contract", entry, data)
		contractData[path] = data
	}

	wantCurrentFiles := []string{
		".github/CODEOWNERS",
		"engdocs/plans/reconciler-redesign/EXTERNAL-REVIEW-fable-10axis.md",
		"engdocs/plans/reconciler-redesign/POST_INTEGRATION_REVIEW.md",
		"test/docsync/reconciler_candidate_manifest_test.go",
	}
	currentFiles := exactFileSet(t, "current", manifest.CurrentFiles, wantCurrentFiles)
	for _, path := range wantCurrentFiles {
		entry := currentFiles[path]
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("reading current file %s: %v", path, err)
		}
		assertCandidateFileHash(t, "current", entry, data)
	}

	wantSections := map[string]candidateSectionSpec{
		"P1.0A":        {"engdocs/plans/reconciler-redesign/IMPLEMENTATION_PLAN.md", "heading", "### P1.0A "},
		"P1.0C":        {"engdocs/plans/reconciler-redesign/IMPLEMENTATION_PLAN.md", "heading", "### P1.0C "},
		"P1.0D":        {"engdocs/plans/reconciler-redesign/IMPLEMENTATION_PLAN.md", "heading", "### P1.0D "},
		"P1.1A":        {"engdocs/plans/reconciler-redesign/IMPLEMENTATION_PLAN.md", "heading", "### P1.1A "},
		"RC-STATE-001": {"engdocs/plans/reconciler-redesign/ACCEPTANCE_MATRIX.md", "heading", "### RC-STATE-001 "},
		"RC-CLI-004":   {"engdocs/plans/reconciler-redesign/ACCEPTANCE_MATRIX.md", "heading", "### RC-CLI-004 "},
		"RC-CLI-005":   {"engdocs/plans/reconciler-redesign/ACCEPTANCE_MATRIX.md", "heading", "### RC-CLI-005 "},
		"RC-SHUT-001":  {"engdocs/plans/reconciler-redesign/ACCEPTANCE_MATRIX.md", "heading", "### RC-SHUT-001 "},
		"RC-SHUT-002":  {"engdocs/plans/reconciler-redesign/ACCEPTANCE_MATRIX.md", "heading", "### RC-SHUT-002 "},
		"RC-SHUT-003":  {"engdocs/plans/reconciler-redesign/ACCEPTANCE_MATRIX.md", "heading", "### RC-SHUT-003 "},
		"RC-SHUT-005":  {"engdocs/plans/reconciler-redesign/ACCEPTANCE_MATRIX.md", "heading", "### RC-SHUT-005 "},
		"RC-EVENT-001": {"engdocs/plans/reconciler-redesign/ACCEPTANCE_MATRIX.md", "heading", "### RC-EVENT-001 "},
		"RC-EVENT-002": {"engdocs/plans/reconciler-redesign/ACCEPTANCE_MATRIX.md", "heading", "### RC-EVENT-002 "},
		"RC-EVENT-003": {"engdocs/plans/reconciler-redesign/ACCEPTANCE_MATRIX.md", "heading", "### RC-EVENT-003 "},
		"RC-CRASH-002": {"engdocs/plans/reconciler-redesign/ACCEPTANCE_MATRIX.md", "heading", "### RC-CRASH-002 "},
		"RC-GATE-001":  {"engdocs/plans/reconciler-redesign/ACCEPTANCE_MATRIX.md", "heading", "### RC-GATE-001 "},
		"RC-PERF-001":  {"engdocs/plans/reconciler-redesign/ACCEPTANCE_MATRIX.md", "heading", "### RC-PERF-001 "},
		"RC-PERF-002":  {"engdocs/plans/reconciler-redesign/ACCEPTANCE_MATRIX.md", "heading", "### RC-PERF-002 "},
		"INV-18":       {"engdocs/plans/reconciler-redesign/IMPLEMENTATION_PLAN.md", "numbered-invariant", "18. **INV-18**"},
		"INV-19":       {"engdocs/plans/reconciler-redesign/IMPLEMENTATION_PLAN.md", "numbered-invariant", "19. **INV-19**"},
		"INV-25":       {"engdocs/plans/reconciler-redesign/IMPLEMENTATION_PLAN.md", "numbered-invariant", "25. **INV-25**"},
	}
	sections := exactSectionSet(t, manifest.Sections, wantSections)
	for id, spec := range wantSections {
		baseCanonical, err := extractCandidateSection(string(contractData[spec.Path]), spec.Kind, spec.Selector)
		if err != nil {
			t.Fatalf("extracting contract section %s: %v", id, err)
		}
		if got := sha256Hex([]byte(baseCanonical)); got != sections[id].SHA256 {
			t.Errorf("contract section %s sha256 = %s, manifest has %s", id, got, sections[id].SHA256)
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(spec.Path)))
		if err != nil {
			t.Fatalf("reading section file %s: %v", spec.Path, err)
		}
		canonical, err := extractCandidateSection(string(data), spec.Kind, spec.Selector)
		if err != nil {
			t.Fatalf("extracting %s: %v", id, err)
		}
		if got := sha256Hex([]byte(canonical)); got != sections[id].SHA256 {
			t.Errorf("section %s sha256 = %s, manifest has %s", id, got, sections[id].SHA256)
		}
	}

	wantGlobalConstraints := []string{"no-command", "no-new-owner", "no-provider-effect-path", "no-schema"}
	if !reflect.DeepEqual(manifest.GlobalConstraints, wantGlobalConstraints) {
		t.Errorf("global constraints = %v, want %v", manifest.GlobalConstraints, wantGlobalConstraints)
	}

	wantSlices := map[string]reconcilerCandidateSlice{
		"P1.0A": {
			Task: "P1.0A", Bead: "ga-f7v2ft.4", TaskHash: sections["P1.0A"].SHA256,
			RowHashes: candidateHashes(sections, "RC-GATE-001", "RC-PERF-001", "RC-PERF-002"), InvariantHashes: []reconcilerCandidateHash{}, NonClaims: []string{},
		},
		"P1.0C": {
			Task: "P1.0C", Bead: "ga-f7v2ft.1", TaskHash: sections["P1.0C"].SHA256,
			RowHashes: candidateHashes(sections, "RC-GATE-001", "RC-STATE-001"), InvariantHashes: candidateHashes(sections, "INV-19"), NonClaims: []string{},
		},
		"P1.0D": {
			Task: "P1.0D", Bead: "ga-f7v2ft.2", TaskHash: sections["P1.0D"].SHA256,
			RowHashes: candidateHashes(sections, "RC-CLI-004", "RC-CLI-005", "RC-GATE-001", "RC-SHUT-001", "RC-SHUT-002", "RC-SHUT-003", "RC-SHUT-005"), InvariantHashes: candidateHashes(sections, "INV-18"),
			NonClaims: []string{"cross-path-exclusion", "durable-acceptance", "operation-lookup", "provider-global-admission"},
		},
		"P1.1A": {
			Task: "P1.1A", Bead: "ga-f7v2ft.5", TaskHash: sections["P1.1A"].SHA256,
			RowHashes:       candidateHashes(sections, "RC-CRASH-002", "RC-EVENT-001", "RC-EVENT-002", "RC-EVENT-003", "RC-GATE-001"),
			InvariantHashes: candidateHashes(sections, "INV-25"), NonClaims: []string{"closed-root-discovery", "guaranteed-publication"},
		},
	}
	exactSliceSet(t, manifest.Slices, wantSlices)

	externalReview := contractFiles["engdocs/plans/reconciler-redesign/EXTERNAL-REVIEW-fable-10axis.md"]
	if manifest.Review.Window50EvidenceSHA256 != externalReview.SHA256 {
		t.Errorf("window 50 review hash = %s, contract has %s", manifest.Review.Window50EvidenceSHA256, externalReview.SHA256)
	}
	const wantWindow50Method = "118-agent independent 10-axis review with adversarial verification; findings were integrated but the report predates the exact candidate blobs"
	if manifest.Review.Window50Method != wantWindow50Method {
		t.Errorf("window 50 method = %q, want %q", manifest.Review.Window50Method, wantWindow50Method)
	}
	post := manifest.Review.PostIntegration
	if post.Path != "engdocs/plans/reconciler-redesign/POST_INTEGRATION_REVIEW.md" || post.Reviewer != "final_exact_reviewer" {
		t.Errorf("post-integration review provenance = {%q %q}, want exact reviewed artifact and reviewer", post.Path, post.Reviewer)
	}
	postFile, ok := currentFiles[post.Path]
	if !ok || post.SHA256 != postFile.SHA256 {
		t.Errorf("post-integration review file/hash is not bound to current_files")
	}
	if post.ReviewedContractCommit != manifest.Contract.Commit || post.PlanBlobOID != contractFiles["engdocs/plans/reconciler-redesign/IMPLEMENTATION_PLAN.md"].BlobOID || post.MatrixBlobOID != contractFiles["engdocs/plans/reconciler-redesign/ACCEPTANCE_MATRIX.md"].BlobOID || post.Verdict != "approve" {
		t.Errorf("post-integration review does not approve the exact contract blobs")
	}
	auth := manifest.Review.ImplementationAuthorization
	const wantAuthorizationEvidence = "Thread request to integrate window 50 review, harden the design and acceptance criteria, and implement on a new origin/main worktree."
	if auth.Identity != "workspace-user" || auth.Date != "2026-07-12" || auth.Evidence != wantAuthorizationEvidence || auth.ExactCandidateApproval || auth.Signature != "not-collected-pre-g0" {
		t.Errorf("implementation authorization must not imply exact-candidate or cryptographic approval")
	}
	if manifest.Enforcement.CodeOwner != "@gastownhall/gascity-admin" || manifest.Enforcement.ObservedMainBranchProtection.RequiredCodeOwnerReviews || manifest.Enforcement.ObservedMainBranchProtection.EnforceAdmins {
		t.Errorf("manifest must report the observed absence of enforced code-owner/admin review honestly")
	}
	if manifest.Enforcement.MergePrecondition != "human owner reviews the exact candidate digest before merge or branch protection begins requiring code-owner review" {
		t.Errorf("unexpected merge precondition %q", manifest.Enforcement.MergePrecondition)
	}

	wantNonClaims := []string{
		"automatic-pr-slice-enforcement",
		"cryptographic-owner-signature",
		"exact-candidate-owner-approval",
		"g0-ratification",
		"protected-merge-without-human-review",
	}
	if !reflect.DeepEqual(manifest.NonClaims, wantNonClaims) {
		t.Errorf("manifest nonclaims = %v, want %v", manifest.NonClaims, wantNonClaims)
	}

	wantDigest := manifest.CandidateDigest
	manifest.CandidateDigest = ""
	canonical, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("canonicalizing manifest: %v", err)
	}
	if got := sha256Hex(canonical); got != wantDigest {
		t.Errorf("candidate digest = %s, manifest has %s", got, wantDigest)
	}
}

func TestRejectDuplicateJSONKeys(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "unique nested values", input: `{"a":1,"b":{"c":2},"d":[{"e":3}]}`},
		{name: "duplicate root key", input: `{"a":1,"a":2}`, wantErr: true},
		{name: "duplicate nested key", input: `{"a":{"b":1,"b":2}}`, wantErr: true},
		{name: "trailing value", input: `{"a":1}{"b":2}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := rejectDuplicateJSONKeys([]byte(tt.input))
			if (err != nil) != tt.wantErr {
				t.Fatalf("rejectDuplicateJSONKeys() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestExactGitCommandDisablesReplacementObjects(t *testing.T) {
	t.Setenv("GIT_NO_REPLACE_OBJECTS", "0")
	cmd := exactGitCommand(t.TempDir(), "version")

	count := 0
	for _, value := range cmd.Env {
		if strings.HasPrefix(value, "GIT_NO_REPLACE_OBJECTS=") {
			count++
			if value != "GIT_NO_REPLACE_OBJECTS=1" {
				t.Fatalf("replacement-object guard = %q, want GIT_NO_REPLACE_OBJECTS=1", value)
			}
		}
	}
	if count != 1 {
		t.Fatalf("replacement-object guard count = %d, want exactly 1", count)
	}
}

func exactFileSet(t *testing.T, label string, entries []reconcilerCandidateFile, want []string) map[string]reconcilerCandidateFile {
	t.Helper()
	got := make(map[string]reconcilerCandidateFile, len(entries))
	for _, entry := range entries {
		if _, duplicate := got[entry.Path]; duplicate {
			t.Fatalf("duplicate %s file %q", label, entry.Path)
		}
		got[entry.Path] = entry
	}
	gotPaths := make([]string, 0, len(got))
	for path := range got {
		gotPaths = append(gotPaths, path)
	}
	sort.Strings(gotPaths)
	if !reflect.DeepEqual(gotPaths, want) {
		t.Fatalf("%s files = %v, want exactly %v", label, gotPaths, want)
	}
	return got
}

func exactSectionSet(t *testing.T, entries []reconcilerCandidateSection, want map[string]candidateSectionSpec) map[string]reconcilerCandidateSection {
	t.Helper()
	got := make(map[string]reconcilerCandidateSection, len(entries))
	for _, entry := range entries {
		if _, duplicate := got[entry.ID]; duplicate {
			t.Fatalf("duplicate section %q", entry.ID)
		}
		spec, ok := want[entry.ID]
		if !ok {
			t.Fatalf("unexpected section %q", entry.ID)
		}
		if entry.Path != spec.Path || entry.Kind != spec.Kind || entry.Selector != spec.Selector {
			t.Fatalf("section %s binding = {%s %s %q}, want {%s %s %q}", entry.ID, entry.Path, entry.Kind, entry.Selector, spec.Path, spec.Kind, spec.Selector)
		}
		got[entry.ID] = entry
	}
	if len(got) != len(want) {
		t.Fatalf("section count = %d, want %d", len(got), len(want))
	}
	return got
}

func exactSliceSet(t *testing.T, entries []reconcilerCandidateSlice, want map[string]reconcilerCandidateSlice) {
	t.Helper()
	got := make(map[string]reconcilerCandidateSlice, len(entries))
	for _, entry := range entries {
		if _, duplicate := got[entry.Task]; duplicate {
			t.Fatalf("duplicate slice %q", entry.Task)
		}
		got[entry.Task] = entry
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("slice mapping mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func candidateHashes(sections map[string]reconcilerCandidateSection, ids ...string) []reconcilerCandidateHash {
	result := make([]reconcilerCandidateHash, 0, len(ids))
	for _, id := range ids {
		result = append(result, reconcilerCandidateHash{ID: id, SHA256: sections[id].SHA256})
	}
	return result
}

func assertCandidateFileHash(t *testing.T, label string, entry reconcilerCandidateFile, data []byte) {
	t.Helper()
	if got := sha256Hex(data); got != entry.SHA256 {
		t.Errorf("%s file %s sha256 = %s, manifest has %s", label, entry.Path, got, entry.SHA256)
	}
	if got := gitBlobOID(data); got != entry.BlobOID {
		t.Errorf("%s file %s blob OID = %s, manifest has %s", label, entry.Path, got, entry.BlobOID)
	}
}

func gitFileAtCommit(t *testing.T, root, commit, path string) []byte {
	t.Helper()
	cmd := exactGitCommand(root, "show", commit+":"+path)
	data, err := cmd.Output()
	if err != nil {
		t.Fatalf("reading %s at %s: %v", path, commit, err)
	}
	return data
}

func gitOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exactGitCommand(root, args...)
	data, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(data))
}

func exactGitCommand(root string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_NO_REPLACE_OBJECTS=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_NO_REPLACE_OBJECTS=1")
	return cmd
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanUniqueJSONValue(decoder, "$"); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return fmt.Errorf("trailing JSON: %w", err)
	}
	return nil
}

func scanUniqueJSONValue(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%s has non-string object key", path)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("%s has duplicate key %q", path, key)
			}
			seen[key] = struct{}{}
			if err := scanUniqueJSONValue(decoder, path+"."+key); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return fmt.Errorf("closing object %s: %w", path, err)
		}
	case '[':
		for index := 0; decoder.More(); index++ {
			if err := scanUniqueJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return fmt.Errorf("closing array %s: %w", path, err)
		}
	default:
		return fmt.Errorf("unexpected delimiter %q at %s", delim, path)
	}
	return nil
}

var candidateInvariantStartRE = regexp.MustCompile(`^[0-9]+\. \*\*INV-[0-9]+\*\*`)

func extractCandidateSection(content, kind, selector string) (string, error) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, selector) {
			if start >= 0 {
				return "", fmt.Errorf("selector %q is ambiguous", selector)
			}
			start = i
		}
	}
	if start < 0 {
		return "", fmt.Errorf("selector %q not found", selector)
	}

	end := len(lines)
	switch kind {
	case "heading":
		level := strings.IndexByte(lines[start], ' ')
		if level < 1 || strings.Trim(lines[start][:level], "#") != "" {
			return "", fmt.Errorf("selector %q is not a Markdown heading", selector)
		}
		for i := start + 1; i < len(lines); i++ {
			candidateLevel := strings.IndexByte(lines[i], ' ')
			if candidateLevel >= 1 && candidateLevel <= level && strings.Trim(lines[i][:candidateLevel], "#") == "" {
				end = i
				break
			}
		}
	case "numbered-invariant":
		for i := start + 1; i < len(lines); i++ {
			if candidateInvariantStartRE.MatchString(lines[i]) {
				end = i
				break
			}
		}
	default:
		return "", fmt.Errorf("unknown section kind %q", kind)
	}

	for end > start+1 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return strings.Join(lines[start:end], "\n") + "\n", nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func gitBlobOID(data []byte) string {
	h := sha1.New() //nolint:gosec // This reproduces Git's object ID, not a security decision.
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(data))
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func isLowerHex(value string, length int) bool {
	if len(value) != length || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
