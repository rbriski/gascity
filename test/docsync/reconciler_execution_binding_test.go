package docsync

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // Git blob identity uses SHA-1; security uses SHA-256 below.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const reconcilerExecutionBindingPath = ".github/reconciler-contract/PRE_G0_EXECUTION_BINDING_V2.json"

var errBindingShallowRepository = errors.New("execution binding requires a full repository")

const bindingHistoryCertificationSkipPattern = `^(TestReconcilerPreG0CandidateManifest|TestReconcilerPreG0ExecutionBinding)$`

const bindingGitCommandTimeout = 30 * time.Second

func TestMain(m *testing.M) {
	pattern, err := bindingHistorySkipPatternForEnvironment()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuring reconciler binding history certification: %v\n", err)
		os.Exit(2)
	}
	if pattern != "" {
		flag.Parse()
		skipFlag := flag.Lookup("test.skip")
		if skipFlag == nil {
			fmt.Fprintln(os.Stderr, "setting reconciler binding history skip pattern: testing skip flag is unavailable")
			os.Exit(2)
		}
		if existing := skipFlag.Value.String(); existing != "" {
			pattern = "(?:" + existing + ")|(?:" + pattern + ")"
		}
		if err := flag.Set("test.skip", pattern); err != nil {
			fmt.Fprintf(os.Stderr, "setting reconciler binding history skip pattern: %v\n", err)
			os.Exit(2)
		}
	}
	os.Exit(m.Run())
}

func bindingHistorySkipPatternForEnvironment() (string, error) {
	authoritative := os.Getenv("GC_BINDING_AUTHORITATIVE") == "1"
	shallow, err := bindingGitOutput(executionBindingRepoRoot(), "rev-parse", "--is-shallow-repository")
	if err != nil {
		return "", err
	}
	switch shallow {
	case "true":
		pattern := bindingHistorySkipPattern(authoritative, true)
		if pattern != "" {
			fmt.Fprintln(os.Stderr, "reconciler binding: shallow non-authoritative checkout; ancestry certification is reserved for the full-history preflight-static trust anchor")
		}
		return pattern, nil
	case "false":
		return "", nil
	default:
		return "", fmt.Errorf("git returned unexpected shallow-repository value %q", shallow)
	}
}

func bindingHistorySkipPattern(authoritative, shallow bool) string {
	if !authoritative && shallow {
		return bindingHistoryCertificationSkipPattern
	}
	return ""
}

func executionBindingRepoRoot() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

type reconcilerExecutionBinding struct {
	SchemaVersion        int                               `json:"schema_version"`
	Kind                 string                            `json:"kind"`
	Status               string                            `json:"status"`
	HashAlgorithm        string                            `json:"hash_algorithm"`
	Canonicalization     string                            `json:"canonicalization"`
	Predecessor          executionBindingPredecessor       `json:"predecessor"`
	Lineage              executionBindingLineage           `json:"lineage"`
	ImmutableV1Files     []executionBindingFile            `json:"immutable_v1_files"`
	ConflictResolutions  []executionBindingConflict        `json:"conflict_resolutions"`
	CleanMergeRetention  executionBindingCleanMerge        `json:"clean_merge_retention"`
	CombinedTreeCensus   []executionBindingCensusRow       `json:"combined_tree_census"`
	BootstrapReview      executionBindingBootstrapReview   `json:"bootstrap_review"`
	GateArtifacts        []executionBindingGateArtifact    `json:"gate_artifacts"`
	GateAttestations     []executionBindingGateAttestation `json:"gate_attestations"`
	IntroductionContract executionBindingIntroduction      `json:"introduction_contract"`
	Authorization        executionBindingAuthorization     `json:"authorization"`
	TrustBoundary        executionBindingTrustBoundary     `json:"trust_boundary"`
	BindingDigest        string                            `json:"binding_digest,omitempty"`
}

type executionBindingFile struct {
	Path    string `json:"path"`
	BlobOID string `json:"blob_oid"`
	SHA256  string `json:"sha256"`
}

type executionBindingPredecessorManifest struct {
	CurrentFiles    []executionBindingFile             `json:"current_files"`
	Sections        []executionBindingCandidateSection `json:"sections"`
	CandidateDigest string                             `json:"candidate_digest"`
}

type executionBindingCandidateSection struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Selector string `json:"selector"`
	SHA256   string `json:"sha256"`
}

type executionBindingPredecessor struct {
	Path            string `json:"path"`
	BlobOID         string `json:"blob_oid"`
	SHA256          string `json:"sha256"`
	CandidateDigest string `json:"candidate_digest"`
}

type executionBindingCommit struct {
	Commit string `json:"commit"`
	Tree   string `json:"tree"`
}

type rawBindingCommit struct {
	Tree    string
	Parents []string
}

type executionBindingDelta struct {
	FromCommit  string `json:"from_commit"`
	ToCommit    string `json:"to_commit"`
	CommitCount int    `json:"commit_count"`
}

type executionBindingCandidateSource struct {
	RemoteRef string `json:"remote_ref"`
	Commit    string `json:"commit"`
	Tree      string `json:"tree"`
}

type executionBindingBootstrap struct {
	Commit         string                 `json:"commit"`
	Tree           string                 `json:"tree"`
	OrderedParents []string               `json:"ordered_parents"`
	MergeBase      executionBindingCommit `json:"merge_base"`
}

type executionBindingUnincorporatedUpstream struct {
	MutableRef               string `json:"mutable_ref"`
	ApprovalObservedTip      string `json:"approval_observed_tip"`
	ApprovalObservedTipTree  string `json:"approval_observed_tip_tree"`
	BindingObservedTip       string `json:"binding_observed_tip"`
	BindingObservedTipTree   string `json:"binding_observed_tip_tree"`
	CommitsAfterApprovedBase int    `json:"commits_after_approved_base"`
	MergeBase                string `json:"merge_base"`
	Incorporated             bool   `json:"incorporated"`
	Policy                   string `json:"policy"`
}

type executionBindingLineage struct {
	Repository               string                                 `json:"repository"`
	PredecessorExecutionBase executionBindingCommit                 `json:"predecessor_execution_base"`
	ApprovedExecutionBase    executionBindingCommit                 `json:"approved_execution_base"`
	ExecutionBaseDelta       executionBindingDelta                  `json:"execution_base_delta"`
	CandidateSource          executionBindingCandidateSource        `json:"candidate_source"`
	Bootstrap                executionBindingBootstrap              `json:"bootstrap"`
	UnincorporatedUpstream   executionBindingUnincorporatedUpstream `json:"unincorporated_upstream"`
}

type executionBindingConflict struct {
	Path            string `json:"path"`
	AncestorBlobOID string `json:"ancestor_blob_oid"`
	Parent1BlobOID  string `json:"parent1_blob_oid"`
	Parent2BlobOID  string `json:"parent2_blob_oid"`
	ResultBlobOID   string `json:"result_blob_oid"`
	SHA256          string `json:"sha256"`
	Resolution      string `json:"resolution"`
}

type executionBindingCleanMerge struct {
	Path            string `json:"path"`
	AncestorBlobOID string `json:"ancestor_blob_oid"`
	Parent1BlobOID  string `json:"parent1_blob_oid"`
	Parent2BlobOID  string `json:"parent2_blob_oid"`
	ResultBlobOID   string `json:"result_blob_oid"`
	SHA256          string `json:"sha256"`
	FailureMode     string `json:"failure_mode"`
	Resolution      string `json:"resolution"`
}

type executionBindingCensusRow struct {
	Class    string `json:"class"`
	Resource string `json:"resource"`
	Calls    int    `json:"calls"`
	Files    int    `json:"files"`
}

type executionBindingBootstrapReview struct {
	Path           string `json:"path"`
	BlobOID        string `json:"blob_oid"`
	SHA256         string `json:"sha256"`
	ReviewedCommit string `json:"reviewed_commit"`
	ReviewedTree   string `json:"reviewed_tree"`
	Reviewer       string `json:"reviewer"`
	Verdict        string `json:"verdict"`
	Independent    bool   `json:"independent"`
}

type executionBindingGateArtifact struct {
	Path            string `json:"path"`
	BlobOID         string `json:"blob_oid"`
	SHA256          string `json:"sha256"`
	Role            string `json:"role"`
	ExpectedOutcome string `json:"expected_outcome"`
}

type executionBindingGateAttestation struct {
	ID           string `json:"id"`
	Command      string `json:"command"`
	Outcome      string `json:"outcome"`
	EvidencePath string `json:"evidence_path"`
}

type executionBindingChangedPath struct {
	Status string `json:"status"`
	Path   string `json:"path"`
}

type executionBindingCIHistory struct {
	FullHistoryWorkflows          []executionBindingFullHistoryWorkflow `json:"full_history_workflows"`
	AuthoritativeWorkflow         string                                `json:"authoritative_workflow"`
	AuthoritativeJob              string                                `json:"authoritative_job"`
	TrustAnchor                   string                                `json:"trust_anchor"`
	ShallowNonAuthoritativePolicy string                                `json:"shallow_non_authoritative_policy"`
	FetchDepth                    int                                   `json:"fetch_depth"`
}

type executionBindingFullHistoryWorkflow struct {
	Path string   `json:"path"`
	Jobs []string `json:"jobs"`
}

type executionBindingIntroduction struct {
	ParentCommit                   string                        `json:"parent_commit"`
	ChangedPaths                   []executionBindingChangedPath `json:"changed_paths"`
	CIHistory                      executionBindingCIHistory     `json:"ci_history"`
	IntroductionCommit             string                        `json:"introduction_commit"`
	ForbidPostIntroductionMutation bool                          `json:"forbid_post_introduction_mutation"`
	ImplementationBasePrecondition string                        `json:"implementation_base_precondition"`
}

type executionBindingAuthorization struct {
	Identity                          string   `json:"identity"`
	Date                              string   `json:"date"`
	Evidence                          string   `json:"evidence"`
	ApprovalSubject                   string   `json:"approval_subject"`
	ApprovedBootstrapCommit           string   `json:"approved_bootstrap_commit"`
	ExactApproval                     bool     `json:"exact_approval"`
	CryptographicSignature            string   `json:"cryptographic_signature"`
	BindingIntroductionCommitApproval bool     `json:"binding_introduction_commit_approval"`
	AuthorizedScopes                  []string `json:"authorized_scopes"`
	ExcludedScopes                    []string `json:"excluded_scopes"`
}

type executionBindingTrustBoundary struct {
	ObservedAt                     string `json:"observed_at"`
	CodeownersAdvisory             bool   `json:"codeowners_advisory"`
	RequiredCodeOwnerReviews       bool   `json:"required_code_owner_reviews"`
	EnforceAdmins                  bool   `json:"enforce_admins"`
	RequiredAggregateContext       string `json:"required_aggregate_context"`
	PreflightIndirectlyRequired    bool   `json:"preflight_indirectly_required"`
	RequiredPostIntroductionReview string `json:"required_post_introduction_review"`
	PostIntroductionReviewStatus   string `json:"post_introduction_review_status"`
	FinalTamperControlClaimed      bool   `json:"final_tamper_control_claimed"`
	ReplacementOwner               string `json:"replacement_owner"`
}

var expectedExecutionBindingPredecessor = executionBindingPredecessor{
	Path:            "engdocs/plans/reconciler-redesign/PRE_G0_CANDIDATE_MANIFEST.json",
	BlobOID:         "87e2f7acd36bdca96fcfecd090257409d8e31ea5",
	SHA256:          "4f944f3c23a63308478fa09d8186b7512a2c68d7e7cfed25779ac18ff6bae10a",
	CandidateDigest: "351f8a2f24a29087e4f6286af177569421eea10cb54e102c8768e711df281c26",
}

var expectedExecutionBindingLineage = executionBindingLineage{
	Repository:               "gastownhall/gascity",
	PredecessorExecutionBase: executionBindingCommit{Commit: "5774497b00ecd0a6c072058069b77fc5874268f2", Tree: "41eb14d20527ca0f89677e9e91c9e4ace04a0b70"},
	ApprovedExecutionBase:    executionBindingCommit{Commit: "d36a8ccadf63c9c782b799e2a02ffbfce12c7dd4", Tree: "ce562d1f935593ed592bad70a93fa73a9f8dc25f"},
	ExecutionBaseDelta:       executionBindingDelta{FromCommit: "5774497b00ecd0a6c072058069b77fc5874268f2", ToCommit: "d36a8ccadf63c9c782b799e2a02ffbfce12c7dd4", CommitCount: 64},
	CandidateSource: executionBindingCandidateSource{
		RemoteRef: "refs/heads/feature/reconciler-keyed-preg0-final-20260713",
		Commit:    "8f46e6ed3930f32e2ec59ac0a70b7328558bfa41",
		Tree:      "63102fb1015f61c042025da9ee51d4f3d5d16b9a",
	},
	Bootstrap: executionBindingBootstrap{
		Commit:         "614a8ebd62cf230de226213f302ed3002dddea61",
		Tree:           "2c1c9d4a2aeaf8188793a26554ec7ea5f9b7cc48",
		OrderedParents: []string{"d36a8ccadf63c9c782b799e2a02ffbfce12c7dd4", "8f46e6ed3930f32e2ec59ac0a70b7328558bfa41"},
		MergeBase:      executionBindingCommit{Commit: "4e90d8378949e3bf73b4e67f0f61e5130a87b91e", Tree: "fc55f8cb98a3c197bf345ebf91ac2462af52386b"},
	},
	UnincorporatedUpstream: executionBindingUnincorporatedUpstream{
		MutableRef:               "refs/remotes/origin/main",
		ApprovalObservedTip:      "c93a05d21296d2277d2479b486015aad0c6b6a53",
		ApprovalObservedTipTree:  "154e7814023682048db22cab90a89a0bf4d878b2",
		BindingObservedTip:       "5e2fa2872a6dc0de996397f6face30893a4c14da",
		BindingObservedTipTree:   "253a681ac6ea22265b13f961d4d415de18287418",
		CommitsAfterApprovedBase: 6,
		MergeBase:                "d36a8ccadf63c9c782b799e2a02ffbfce12c7dd4",
		Incorporated:             false,
		Policy:                   "record-only; incorporating any post-d36a8cca upstream commit requires expanded approval",
	},
}

var expectedExecutionBindingImmutableFiles = []executionBindingFile{
	{Path: ".github/CODEOWNERS", BlobOID: "675e7f1cff3ee31e70f7e31d8d5363a85e88d781", SHA256: "a349ea38b1e043cffd57726cf3ec64e1214abad92be219f02b8997b4b123d6b8"},
	{Path: "engdocs/plans/reconciler-redesign/PRE_G0_CANDIDATE_MANIFEST.json", BlobOID: "87e2f7acd36bdca96fcfecd090257409d8e31ea5", SHA256: "4f944f3c23a63308478fa09d8186b7512a2c68d7e7cfed25779ac18ff6bae10a"},
	{Path: "engdocs/plans/reconciler-redesign/POST_INTEGRATION_REVIEW.md", BlobOID: "295d7112e248c68ba0dbedb55352d5bc345b3f2c", SHA256: "eda1522421c01ca746993525ab46899d22bb11c242c8d62bc45e8d38cc8244aa"},
	{Path: "test/docsync/reconciler_candidate_manifest_test.go", BlobOID: "07496a454de90bf1f1cc6727f9ba754ced644ec8", SHA256: "4acb3bf7228925b15b8fa7d98147a2a3e9b4099aefd59da97e541455f55b71b7"},
}

var expectedExecutionBindingConflicts = []executionBindingConflict{
	{
		Path: "TESTING.md", AncestorBlobOID: "afc9ba5f2c872df7db65adc465f4b04d70300d64", Parent1BlobOID: "706bac70bc853f50caea1dd16b9c1c72c0de6c6e", Parent2BlobOID: "b07a2a6fd6804fd7e5cb8108199d0f3fd901e2f6", ResultBlobOID: "62ac13ec9fc3897b01269ee377a7c84fe3e65f3a", SHA256: "47b70db4d2559af3e378339b758a644f0d72a7a32fe91e56a64018bb6ec7a73f", Resolution: "regenerate-from-combined-tree",
	},
	{
		Path: "internal/bdflags/bdflags.go", AncestorBlobOID: "51b0dce94fa53c00faf51002a5ab62e886528d32", Parent1BlobOID: "c033c24662ddb07e0420dfadb8296815b9c84053", Parent2BlobOID: "a5b3e4fb4701627ffaa65711df26e0161dedbe8d", ResultBlobOID: "a5b3e4fb4701627ffaa65711df26e0161dedbe8d", SHA256: "b27235f7eda058a3d47b1a8734d09d5bb4cbb36a6e4ecdcd1127f77e1dc760ed", Resolution: "retain-reviewed-source",
	},
	{
		Path: "internal/testpolicy/resourcecensus/census.go", AncestorBlobOID: "75a350f64da03dc317acec60de3ae1d0e7b26b27", Parent1BlobOID: "6cab055d15bf7ccf2c57e2d64342ade1ce6a1753", Parent2BlobOID: "a7376a05ff0774557dbafbaf29ea65efd0e41cb9", ResultBlobOID: "cf2ad511ce76cc0630edff2ec0b9f46e952db487", SHA256: "643cc7993bba8602fafa5a615d79bd29c9fe5bf99dc7deeade10d498076d1ca0", Resolution: "regenerate-from-combined-tree",
	},
	{
		Path: "test/test-resources.toml", AncestorBlobOID: "5160f48b3a017f9b1c47da9446e04e2a1fdf8e3a", Parent1BlobOID: "5ef568691aa6a47e25eecdaae4f2e736e82620b5", Parent2BlobOID: "dfba27db72bec536df0b102cd052812446ef1991", ResultBlobOID: "5450456ff422c2b0374245dd89d71b2482a4ecc0", SHA256: "4fdbdc31e9cb084207afc88bce1771dd84a9a09ffa7efcd10099589a14ae1a4b", Resolution: "regenerate-from-combined-tree",
	},
}

var expectedExecutionBindingCleanMerge = executionBindingCleanMerge{
	Path:            "internal/bdflags/bdflags_test.go",
	AncestorBlobOID: "fbd4136e694a0e438e5106e1332089a1515aa86e",
	Parent1BlobOID:  "03e41c3ad1f38e012078f837d9d786edfcb64f05",
	Parent2BlobOID:  "a7d6303916ac4a445839de2834f88addcd371e42",
	ResultBlobOID:   "a7d6303916ac4a445839de2834f88addcd371e42",
	SHA256:          "ddca2e0fbae5ff8bf216189e0214848cfaf72d1e471deca7366721f847efcbe9",
	FailureMode:     "automatic-merge-produces-duplicate-test-symbol",
	Resolution:      "retain-stronger-reviewed-source-test",
}

var expectedExecutionBindingCensus = []executionBindingCensusRow{
	{Class: "audit", Resource: "subprocess", Calls: 492, Files: 137},
	{Class: "audit", Resource: "fixed_sleep", Calls: 438, Files: 153},
	{Class: "source-debt", Resource: "subprocess", Calls: 375, Files: 98},
	{Class: "source-debt", Resource: "fixed_sleep", Calls: 286, Files: 110},
	{Class: "source-debt", Resource: "environment", Calls: 4173, Files: 185},
	{Class: "source-debt", Resource: "cwd", Calls: 210, Files: 40},
	{Class: "source-debt", Resource: "slow_process_gate", Calls: 77, Files: 26},
	{Class: "source-debt", Resource: "http_test_server", Calls: 256, Files: 56},
	{Class: "small-debt", Resource: "subprocess", Calls: 375, Files: 98},
	{Class: "small-debt", Resource: "fixed_sleep", Calls: 286, Files: 110},
	{Class: "small-debt", Resource: "environment", Calls: 4167, Files: 185},
	{Class: "small-debt", Resource: "cwd", Calls: 210, Files: 40},
	{Class: "small-debt", Resource: "slow_process_gate", Calls: 77, Files: 26},
	{Class: "small-debt", Resource: "http_test_server", Calls: 256, Files: 56},
}

var expectedExecutionBindingBootstrapReview = executionBindingBootstrapReview{
	Path:           ".github/reconciler-contract/POST_BOOTSTRAP_REVIEW.md",
	BlobOID:        "5581a1721c41f28535ca70d6e150527e388205f0",
	SHA256:         "623677b07336fbefc8abd0567b823ee3d1eeb87885cf7654beb67904be8f0110",
	ReviewedCommit: "614a8ebd62cf230de226213f302ed3002dddea61",
	ReviewedTree:   "2c1c9d4a2aeaf8188793a26554ec7ea5f9b7cc48",
	Reviewer:       "independent-ga-f7v2ft.12",
	Verdict:        "approve",
	Independent:    true,
}

var expectedExecutionBindingGateAttestations = []executionBindingGateAttestation{
	{ID: "candidate-manifest", Command: "go test -mod=readonly -count=1 ./test/docsync -run '^TestReconcilerPreG0CandidateManifest$'", Outcome: "pass", EvidencePath: ".github/reconciler-contract/POST_BOOTSTRAP_REVIEW.md"},
	{ID: "resource-census", Command: "go test -mod=readonly -count=1 ./internal/testpolicy/resourcecensus -run '^TestRepositoryLedgerMatchesCensusAndDocumentation$'", Outcome: "pass", EvidencePath: ".github/reconciler-contract/POST_BOOTSTRAP_REVIEW.md"},
	{ID: "bdflags", Command: "go test -mod=readonly -count=1 ./internal/bdflags", Outcome: "pass", EvidencePath: ".github/reconciler-contract/POST_BOOTSTRAP_REVIEW.md"},
	{ID: "fast-unit", Command: "make test-fast-parallel", Outcome: "pass", EvidencePath: ".github/reconciler-contract/POST_BOOTSTRAP_REVIEW.md"},
	{ID: "cmd-gc-process", Command: "make test-cmd-gc-process-parallel", Outcome: "pass", EvidencePath: ".github/reconciler-contract/POST_BOOTSTRAP_REVIEW.md"},
	{ID: "vet", Command: "go vet ./...", Outcome: "pass", EvidencePath: ".github/reconciler-contract/POST_BOOTSTRAP_REVIEW.md"},
	{ID: "pre-commit", Command: ".githooks/pre-commit", Outcome: "pass", EvidencePath: ".github/reconciler-contract/POST_BOOTSTRAP_REVIEW.md"},
}

var expectedExecutionBindingIntroduction = executionBindingIntroduction{
	ParentCommit: "614a8ebd62cf230de226213f302ed3002dddea61",
	ChangedPaths: []executionBindingChangedPath{
		{Status: "A", Path: ".github/reconciler-contract/PRE_G0_EXECUTION_BINDING_V2.json"},
		{Status: "A", Path: ".github/reconciler-contract/POST_BOOTSTRAP_REVIEW.md"},
		{Status: "M", Path: ".github/workflows/ci.yml"},
		{Status: "M", Path: "TESTING.md"},
		{Status: "M", Path: "internal/testpolicy/resourcecensus/census.go"},
		{Status: "A", Path: "test/docsync/reconciler_execution_binding_test.go"},
		{Status: "M", Path: "test/test-resources.toml"},
	},
	CIHistory: executionBindingCIHistory{
		FullHistoryWorkflows: []executionBindingFullHistoryWorkflow{
			{Path: ".github/workflows/ci.yml", Jobs: []string{"preflight-static"}},
		},
		AuthoritativeWorkflow:         ".github/workflows/ci.yml",
		AuthoritativeJob:              "preflight-static",
		TrustAnchor:                   "protected-early-single-file-stdlib-verifier-runs-uncached",
		ShallowNonAuthoritativePolicy: "skip-exact-ancestry-roots-run-current-artifact-checks",
		FetchDepth:                    0,
	},
	IntroductionCommit:             "derived-not-embedded",
	ForbidPostIntroductionMutation: true,
	ImplementationBasePrecondition: "independent-ga-f7v2ft.13-approve",
}

var expectedExecutionBindingAuthorization = executionBindingAuthorization{
	Identity:                          "workspace-user",
	Date:                              "2026-07-14",
	Evidence:                          "Thread message: I approve. Let's make it happen!",
	ApprovalSubject:                   "exact-pinned-bootstrap-and-default-inert-phase-0-evidence",
	ApprovedBootstrapCommit:           "614a8ebd62cf230de226213f302ed3002dddea61",
	ExactApproval:                     true,
	CryptographicSignature:            "not-collected-execution-binding",
	BindingIntroductionCommitApproval: false,
	AuthorizedScopes:                  []string{"append-only-bootstrap", "default-inert-phase-0-evidence"},
	ExcludedScopes:                    []string{"operational-g0", "production-owner-cutover", "provider-effects", "runtime-action-concurrency", "schema-mutation"},
}

var expectedExecutionBindingTrustBoundary = executionBindingTrustBoundary{
	ObservedAt:                     "2026-07-14",
	CodeownersAdvisory:             true,
	RequiredCodeOwnerReviews:       false,
	EnforceAdmins:                  false,
	RequiredAggregateContext:       "Check",
	PreflightIndirectlyRequired:    true,
	RequiredPostIntroductionReview: "ga-f7v2ft.13",
	PostIntroductionReviewStatus:   "pending-at-introduction",
	FinalTamperControlClaimed:      false,
	ReplacementOwner:               "P0.16",
}

func TestReconcilerPreG0ExecutionBinding(t *testing.T) {
	root := executionBindingRepoRoot()
	data, err := os.ReadFile(filepath.Join(root, reconcilerExecutionBindingPath))
	if err != nil {
		t.Fatalf("reading %s: %v", reconcilerExecutionBindingPath, err)
	}
	if err := validateReconcilerExecutionBinding(root, data); err != nil {
		t.Fatalf("validating %s: %v", reconcilerExecutionBindingPath, err)
	}
}

func TestReconcilerExecutionBindingCurrentArtifacts(t *testing.T) {
	root := executionBindingRepoRoot()
	data, err := os.ReadFile(filepath.Join(root, reconcilerExecutionBindingPath))
	if err != nil {
		t.Fatalf("reading %s: %v", reconcilerExecutionBindingPath, err)
	}
	if _, err := validateReconcilerExecutionBindingCurrentState(root, data); err != nil {
		t.Fatalf("validating current binding state: %v", err)
	}
}

func TestReconcilerExecutionBindingRejectsMalformedJSON(t *testing.T) {
	data := readExecutionBindingForTest(t)
	tests := []struct {
		name string
		data []byte
	}{
		{name: "unknown root field", data: []byte(strings.Replace(string(data), "{", `{"unknown":true,`, 1))},
		{name: "unknown nested field", data: []byte(strings.Replace(string(data), `"predecessor": {`, `"predecessor":{"unknown":true,`, 1))},
		{name: "case alias root field", data: []byte(strings.Replace(string(data), "{", `{"SCHEMA_VERSION":1,`, 1))},
		{name: "case alias authorization field", data: []byte(strings.Replace(string(data), `"authorized_scopes": [`, `"AUTHORIZED_SCOPES":["operational-g0"],"authorized_scopes":[`, 1))},
		{name: "duplicate root key", data: []byte(strings.Replace(string(data), "{", `{"schema_version":2,`, 1))},
		{name: "duplicate nested key", data: []byte(strings.Replace(string(data), `"predecessor": {`, `"predecessor":{"path":"duplicate",`, 1))},
		{name: "null false field", data: []byte(strings.Replace(string(data), `"incorporated": false`, `"incorporated": null`, 1))},
		{name: "null zero field", data: []byte(strings.Replace(string(data), `"fetch_depth": 0`, `"fetch_depth": null`, 1))},
		{name: "omitted false field", data: []byte(strings.Replace(string(data), "      \"incorporated\": false,\n", "", 1))},
		{name: "trailing value", data: append(append([]byte(nil), data...), []byte(`{"trailing":true}`)...)},
		{name: "trailing malformed data", data: append(append([]byte(nil), data...), '[')},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeReconcilerExecutionBinding(tt.data); err == nil {
				t.Fatal("malformed binding unexpectedly decoded")
			}
		})
	}
}

func TestReconcilerExecutionBindingRejectsRecomputedSemanticMutations(t *testing.T) {
	root := executionBindingRepoRoot()
	valid := readExecutionBindingForTest(t)
	tests := []struct {
		name   string
		mutate func(*reconcilerExecutionBinding)
	}{
		{name: "operational status", mutate: func(b *reconcilerExecutionBinding) { b.Status = "operational" }},
		{name: "predecessor digest", mutate: func(b *reconcilerExecutionBinding) { b.Predecessor.CandidateDigest = strings.Repeat("0", 64) }},
		{name: "predecessor blob", mutate: func(b *reconcilerExecutionBinding) { b.Predecessor.BlobOID = strings.Repeat("0", 40) }},
		{name: "later main substituted as base", mutate: func(b *reconcilerExecutionBinding) {
			b.Lineage.ApprovedExecutionBase.Commit = "c93a05d21296d2277d2479b486015aad0c6b6a53"
		}},
		{name: "wrong candidate source", mutate: func(b *reconcilerExecutionBinding) {
			b.Lineage.CandidateSource.Commit = b.Lineage.ApprovedExecutionBase.Commit
		}},
		{name: "wrong bootstrap tree", mutate: func(b *reconcilerExecutionBinding) { b.Lineage.Bootstrap.Tree = b.Lineage.ApprovedExecutionBase.Tree }},
		{name: "reversed parents", mutate: func(b *reconcilerExecutionBinding) {
			b.Lineage.Bootstrap.OrderedParents[0], b.Lineage.Bootstrap.OrderedParents[1] = b.Lineage.Bootstrap.OrderedParents[1], b.Lineage.Bootstrap.OrderedParents[0]
		}},
		{name: "missing parent", mutate: func(b *reconcilerExecutionBinding) {
			b.Lineage.Bootstrap.OrderedParents = b.Lineage.Bootstrap.OrderedParents[:1]
		}},
		{name: "extra parent", mutate: func(b *reconcilerExecutionBinding) {
			b.Lineage.Bootstrap.OrderedParents = append(b.Lineage.Bootstrap.OrderedParents, b.Lineage.Bootstrap.MergeBase.Commit)
		}},
		{name: "upstream silently incorporated", mutate: func(b *reconcilerExecutionBinding) { b.Lineage.UnincorporatedUpstream.Incorporated = true }},
		{name: "immutable file hash", mutate: func(b *reconcilerExecutionBinding) { b.ImmutableV1Files[0].SHA256 = strings.Repeat("0", 64) }},
		{name: "duplicate immutable file", mutate: func(b *reconcilerExecutionBinding) {
			b.ImmutableV1Files = append(b.ImmutableV1Files, b.ImmutableV1Files[0])
		}},
		{name: "conflict result", mutate: func(b *reconcilerExecutionBinding) {
			b.ConflictResolutions[0].ResultBlobOID = b.ConflictResolutions[0].Parent1BlobOID
		}},
		{name: "duplicate conflict", mutate: func(b *reconcilerExecutionBinding) {
			b.ConflictResolutions = append(b.ConflictResolutions, b.ConflictResolutions[0])
		}},
		{name: "clean merge trap removed", mutate: func(b *reconcilerExecutionBinding) { b.CleanMergeRetention = executionBindingCleanMerge{} }},
		{name: "source census environment", mutate: func(b *reconcilerExecutionBinding) { b.CombinedTreeCensus[4].Calls = 4167 }},
		{name: "bootstrap review rejected", mutate: func(b *reconcilerExecutionBinding) { b.BootstrapReview.Verdict = "reject" }},
		{name: "bootstrap review hash", mutate: func(b *reconcilerExecutionBinding) { b.BootstrapReview.SHA256 = strings.Repeat("0", 64) }},
		{name: "bootstrap review omitted", mutate: func(b *reconcilerExecutionBinding) { b.BootstrapReview = executionBindingBootstrapReview{} }},
		{name: "gate artifact hash", mutate: func(b *reconcilerExecutionBinding) { b.GateArtifacts[0].SHA256 = strings.Repeat("0", 64) }},
		{name: "gate expected failure", mutate: func(b *reconcilerExecutionBinding) { b.GateArtifacts[0].ExpectedOutcome = "fail" }},
		{name: "gate artifact omitted", mutate: func(b *reconcilerExecutionBinding) { b.GateArtifacts = b.GateArtifacts[1:] }},
		{name: "failed attestation", mutate: func(b *reconcilerExecutionBinding) { b.GateAttestations[0].Outcome = "fail" }},
		{name: "skipped attestation", mutate: func(b *reconcilerExecutionBinding) { b.GateAttestations[0].Outcome = "skip" }},
		{name: "wrong attestation command", mutate: func(b *reconcilerExecutionBinding) { b.GateAttestations[0].Command = "true" }},
		{name: "duplicate attestation", mutate: func(b *reconcilerExecutionBinding) {
			b.GateAttestations = append(b.GateAttestations, b.GateAttestations[0])
		}},
		{name: "wrong introduction parent", mutate: func(b *reconcilerExecutionBinding) {
			b.IntroductionContract.ParentCommit = b.Lineage.ApprovedExecutionBase.Commit
		}},
		{name: "extra introduction path", mutate: func(b *reconcilerExecutionBinding) {
			b.IntroductionContract.ChangedPaths = append(b.IntroductionContract.ChangedPaths, executionBindingChangedPath{Status: "A", Path: "extra"})
		}},
		{name: "authoritative history job changed", mutate: func(b *reconcilerExecutionBinding) {
			b.IntroductionContract.CIHistory.AuthoritativeJob = "integration-shards"
		}},
		{name: "shallow lane policy removed", mutate: func(b *reconcilerExecutionBinding) {
			b.IntroductionContract.CIHistory.ShallowNonAuthoritativePolicy = ""
		}},
		{name: "authoritative history made shallow", mutate: func(b *reconcilerExecutionBinding) {
			b.IntroductionContract.CIHistory.FetchDepth = 1
		}},
		{name: "post-introduction mutation allowed", mutate: func(b *reconcilerExecutionBinding) { b.IntroductionContract.ForbidPostIntroductionMutation = false }},
		{name: "exact approval removed", mutate: func(b *reconcilerExecutionBinding) { b.Authorization.ExactApproval = false }},
		{name: "signature overstated", mutate: func(b *reconcilerExecutionBinding) { b.Authorization.CryptographicSignature = "signed" }},
		{name: "binding commit self-approved", mutate: func(b *reconcilerExecutionBinding) { b.Authorization.BindingIntroductionCommitApproval = true }},
		{name: "authorized scope broadened", mutate: func(b *reconcilerExecutionBinding) {
			b.Authorization.AuthorizedScopes = append(b.Authorization.AuthorizedScopes, "operational-g0")
		}},
		{name: "excluded scope removed", mutate: func(b *reconcilerExecutionBinding) {
			b.Authorization.ExcludedScopes = b.Authorization.ExcludedScopes[:len(b.Authorization.ExcludedScopes)-1]
		}},
		{name: "codeowner enforcement overstated", mutate: func(b *reconcilerExecutionBinding) { b.TrustBoundary.RequiredCodeOwnerReviews = true }},
		{name: "final tamper control overstated", mutate: func(b *reconcilerExecutionBinding) { b.TrustBoundary.FinalTamperControlClaimed = true }},
		{name: "required post-introduction review removed", mutate: func(b *reconcilerExecutionBinding) { b.TrustBoundary.RequiredPostIntroductionReview = "" }},
		{name: "post-introduction review prematurely approved", mutate: func(b *reconcilerExecutionBinding) { b.TrustBoundary.PostIntroductionReviewStatus = "approve" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binding := cloneExecutionBinding(t, valid)
			tt.mutate(binding)
			mutated := marshalExecutionBindingWithFreshDigest(t, binding)
			if _, err := validateReconcilerExecutionBindingCurrentState(root, mutated); err == nil {
				t.Fatal("semantic mutation with recomputed digest unexpectedly validated")
			}
		})
	}
}

func TestReconcilerExecutionBindingRejectsInvalidDigest(t *testing.T) {
	data := readExecutionBindingForTest(t)
	for _, digest := range []string{strings.Repeat("0", 64), strings.ToUpper(strings.Repeat("a", 64)), "short"} {
		binding := cloneExecutionBinding(t, data)
		binding.BindingDigest = digest
		mutated, err := json.Marshal(binding)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateReconcilerExecutionBindingSemantics(binding); err == nil {
			t.Fatalf("invalid digest %q unexpectedly validated; encoded fixture: %s", digest, mutated)
		}
	}
}

func TestReconcilerExecutionBindingRejectsShallowRepository(t *testing.T) {
	parent := t.TempDir()
	origin := filepath.Join(parent, "origin")
	if err := os.Mkdir(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	runBindingGitTest(t, origin, "init", "-q")
	runBindingGitTest(t, origin, "config", "user.name", "Binding Test")
	runBindingGitTest(t, origin, "config", "user.email", "binding@example.invalid")
	writeBindingTestFile(t, origin, "value.txt", "one\n")
	runBindingGitTest(t, origin, "add", "value.txt")
	runBindingGitTest(t, origin, "commit", "-q", "-m", "one")
	writeBindingTestFile(t, origin, "value.txt", "two\n")
	runBindingGitTest(t, origin, "commit", "-q", "-am", "two")

	shallow := filepath.Join(parent, "shallow")
	runBindingGitTest(t, parent, "clone", "-q", "--depth=1", "file://"+origin, shallow)
	if err := requireFullBindingHistory(shallow); err == nil {
		t.Fatal("shallow repository unexpectedly accepted")
	}
}

func TestReconcilerExecutionBindingRejectsMissingObject(t *testing.T) {
	repo := t.TempDir()
	runBindingGitTest(t, repo, "init", "-q")
	err := validateExactBindingCommit(repo, "missing", executionBindingCommit{Commit: strings.Repeat("0", 40), Tree: strings.Repeat("0", 40)})
	if err == nil {
		t.Fatal("missing commit object unexpectedly accepted")
	}
}

func TestReconcilerExecutionBindingRejectsGitGrafts(t *testing.T) {
	repo := t.TempDir()
	runBindingGitTest(t, repo, "init", "-q")
	runBindingGitTest(t, repo, "config", "user.name", "Binding Test")
	runBindingGitTest(t, repo, "config", "user.email", "binding@example.invalid")
	writeBindingTestFile(t, repo, "value.txt", "one\n")
	runBindingGitTest(t, repo, "add", "value.txt")
	runBindingGitTest(t, repo, "commit", "-q", "-m", "one")
	ancestor, err := bindingGitOutput(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	writeBindingTestFile(t, repo, "value.txt", "two\n")
	runBindingGitTest(t, repo, "commit", "-q", "-am", "two")
	writeBindingTestFile(t, repo, "value.txt", "three\n")
	runBindingGitTest(t, repo, "commit", "-q", "-am", "three")
	head, err := bindingGitOutput(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	gitDir, err := bindingGitOutput(repo, "rev-parse", "--git-dir")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repo, gitDir)
	}
	writeBindingTestFile(t, gitDir, "info/grafts", head+" "+ancestor+"\n")
	if err := requireFullBindingHistory(repo); err == nil {
		t.Fatal("repository with legacy Git grafts unexpectedly accepted")
	}
}

func TestReconcilerExecutionBindingFindsCommittedIntroduction(t *testing.T) {
	repo := t.TempDir()
	runBindingGitTest(t, repo, "init", "-q")
	runBindingGitTest(t, repo, "config", "user.name", "Binding Test")
	runBindingGitTest(t, repo, "config", "user.email", "binding@example.invalid")
	writeBindingTestFile(t, repo, "base.txt", "base\n")
	runBindingGitTest(t, repo, "add", "base.txt")
	runBindingGitTest(t, repo, "commit", "-q", "-m", "base")
	writeBindingTestFile(t, repo, reconcilerExecutionBindingPath, "{}\n")
	runBindingGitTest(t, repo, "add", reconcilerExecutionBindingPath)
	runBindingGitTest(t, repo, "commit", "-q", "-m", "introduce binding")
	want, err := bindingGitOutput(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	got, err := bindingIntroductionCommits(repo, reconcilerExecutionBindingPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{want}) {
		t.Fatalf("binding introductions = %v, want [%s]", got, want)
	}
}

func TestReconcilerExecutionBindingRejectsMutateThenRevertHistory(t *testing.T) {
	repo := t.TempDir()
	runBindingGitTest(t, repo, "init", "-q")
	runBindingGitTest(t, repo, "config", "user.name", "Binding Test")
	runBindingGitTest(t, repo, "config", "user.email", "binding@example.invalid")
	mainBranch, err := bindingGitOutput(repo, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	const path = "contract.json"
	introduced := []byte("{\"value\":1}\n")
	writeBindingTestFile(t, repo, path, string(introduced))
	runBindingGitTest(t, repo, "add", path)
	runBindingGitTest(t, repo, "commit", "-q", "-m", "base")
	runBindingGitTest(t, repo, "branch", "side")
	runBindingGitTest(t, repo, "commit", "-q", "--allow-empty", "-m", "introduce")
	introduction, err := bindingGitOutput(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	runBindingGitTest(t, repo, "checkout", "-q", "side")
	writeBindingTestFile(t, repo, path, "{\"value\":2}\n")
	runBindingGitTest(t, repo, "commit", "-q", "-am", "mutate")
	writeBindingTestFile(t, repo, path, string(introduced))
	runBindingGitTest(t, repo, "commit", "-q", "-am", "revert")
	runBindingGitTest(t, repo, "checkout", "-q", mainBranch)
	runBindingGitTest(t, repo, "merge", "-q", "--no-ff", "side", "-m", "merge reverted side history")

	if err := validateBindingPathUnchanged(repo, introduction, path, introduced); err == nil {
		t.Fatal("pre-introduction-forked mutate-then-revert history unexpectedly accepted")
	}
}

func TestScrubBindingGitEnvironment(t *testing.T) {
	got := scrubBindingGitEnvironment([]string{
		"PATH=/usr/bin",
		"HOME=/tmp/home",
		"GIT_DIR=/attacker",
		"GIT_WORK_TREE=/attacker",
		"GIT_OBJECT_DIRECTORY=/attacker",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=/attacker",
		"GIT_INDEX_FILE=/attacker",
		"GIT_SHALLOW_FILE=/attacker",
		"GIT_NO_REPLACE_OBJECTS=0",
		"GIT_CONFIG_NOSYSTEM=0",
		"GIT_CONFIG_GLOBAL=/attacker",
		"GIT_TERMINAL_PROMPT=1",
		"GIT_NO_LAZY_FETCH=0",
	})
	want := []string{
		"PATH=/usr/bin",
		"HOME=/tmp/home",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_OPTIONAL_LOCKS=0",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scrubbed Git environment = %v, want %v", got, want)
	}
}

func TestWorkflowWithBindingTrustAnchorIsEarlyAndPinned(t *testing.T) {
	const verifierBlob = "0123456789abcdef0123456789abcdef01234567"
	const verifierSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	workflow := []byte(`jobs:
  preflight-static:
    steps:
      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6
        with:
          fetch-depth: 0
      - uses: actions/setup-go@4a3601121dd01d1626a1e23e37211e3254c1c06c # v6
        with:
          go-version-file: go.mod
      - name: go.mod replace guard
        run: make check-gomod-replace
      - name: Docs
        run: make check-docs
`)

	gotBytes, err := workflowWithBindingTrustAnchor(workflow, verifierBlob, verifierSHA256)
	if err != nil {
		t.Fatal(err)
	}
	got := string(gotBytes)
	anchorIndex := strings.Index(got, "      - name: Reconciler execution binding trust anchor\n")
	setupIndex := strings.Index(got, "      - uses: actions/setup-go@")
	firstRepositoryCommandIndex := strings.Index(got, "      - name: go.mod replace guard\n")
	if anchorIndex < 0 || setupIndex < 0 || firstRepositoryCommandIndex < 0 {
		t.Fatalf("workflow is missing setup, trust anchor, or first repository command:\n%s", got)
	}
	if anchorIndex <= setupIndex || anchorIndex >= firstRepositoryCommandIndex {
		t.Fatalf("trust anchor must run immediately after setup-go and before repository commands:\n%s", got)
	}

	required := []string{
		`test "$(git rev-parse --is-shallow-repository)" = "false"`,
		`test "$(git hash-object --no-filters test/docsync/reconciler_execution_binding_test.go)" = "` + verifierBlob + `"`,
		`test "$(sha256sum test/docsync/reconciler_execution_binding_test.go | awk '{print $1}')" = "` + verifierSHA256 + `"`,
		"GC_BINDING_AUTHORITATIVE=1 GOWORK=off GOFLAGS= GOENV=off GOTOOLCHAIN=local CGO_ENABLED=0",
		"go test -mod=readonly -count=1",
		"test/docsync/reconciler_execution_binding_test.go",
		"-run '^(TestReconciler.*|TestScrubBindingGitEnvironment)$'",
	}
	for _, value := range required {
		if !strings.Contains(got, value) {
			t.Errorf("trust anchor is missing %q", value)
		}
	}
	for _, path := range []string{
		"test/docsync/docsync_test.go",
		"test/docsync/reconciler_candidate_manifest_test.go",
	} {
		if strings.Contains(got, path) {
			t.Fatalf("trust anchor compiles an external verifier dependency %s", path)
		}
	}
}

func TestBindingHistorySkipPattern(t *testing.T) {
	tests := []struct {
		name          string
		authoritative bool
		shallow       bool
		want          string
	}{
		{
			name:    "shallow non-authoritative checkout",
			shallow: true,
			want:    `^(TestReconcilerPreG0CandidateManifest|TestReconcilerPreG0ExecutionBinding)$`,
		},
		{name: "authoritative lane fails closed", authoritative: true, shallow: true},
		{name: "full checkout runs certification"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bindingHistorySkipPattern(tt.authoritative, tt.shallow)
			if got != tt.want {
				t.Fatalf("skip pattern = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractExecutionBindingCandidateSection(t *testing.T) {
	content := strings.Join([]string{
		"# Root",
		"",
		"### P1.0A First slice",
		"body",
		"",
		"#### Nested detail",
		"nested body",
		"",
		"### P1.0B Second slice",
		"other body",
		"",
		"18. **INV-18** First invariant",
		"invariant body",
		"",
		"19. **INV-19** Second invariant",
		"next body",
		"",
	}, "\n")
	tests := []struct {
		name, kind, selector, want string
	}{
		{
			name:     "heading includes nested headings",
			kind:     "heading",
			selector: "### P1.0A ",
			want:     "### P1.0A First slice\nbody\n\n#### Nested detail\nnested body\n",
		},
		{
			name:     "numbered invariant stops at next invariant",
			kind:     "numbered-invariant",
			selector: "18. **INV-18**",
			want:     "18. **INV-18** First invariant\ninvariant body\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractExecutionBindingCandidateSection(content, tt.kind, tt.selector)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("section = %q, want %q", got, tt.want)
			}
		})
	}
}

func readExecutionBindingForTest(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(executionBindingRepoRoot(), reconcilerExecutionBindingPath))
	if err != nil {
		t.Fatalf("reading %s: %v", reconcilerExecutionBindingPath, err)
	}
	return data
}

func cloneExecutionBinding(t *testing.T, data []byte) *reconcilerExecutionBinding {
	t.Helper()
	binding, err := decodeReconcilerExecutionBinding(data)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	clone, err := decodeReconcilerExecutionBinding(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return clone
}

func marshalExecutionBindingWithFreshDigest(t *testing.T, binding *reconcilerExecutionBinding) []byte {
	t.Helper()
	binding.BindingDigest = ""
	canonical, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	binding.BindingDigest = executionBindingSHA256Hex(canonical)
	data, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func runBindingGitTest(t *testing.T, root string, args ...string) {
	t.Helper()
	if _, err := bindingGitBytes(root, args...); err != nil {
		t.Fatal(err)
	}
}

func writeBindingTestFile(t *testing.T, root, path, content string) {
	t.Helper()
	fullPath := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func validateReconcilerExecutionBinding(root string, data []byte) error {
	binding, err := validateReconcilerExecutionBindingCurrentState(root, data)
	if err != nil {
		return err
	}
	return validateReconcilerExecutionBindingRepository(root, data, binding)
}

func validateReconcilerExecutionBindingCurrentState(root string, data []byte) (*reconcilerExecutionBinding, error) {
	binding, err := decodeReconcilerExecutionBinding(data)
	if err != nil {
		return nil, err
	}
	if err := validateReconcilerExecutionBindingSemantics(binding); err != nil {
		return nil, err
	}
	if err := validateBindingCurrentArtifacts(root, binding); err != nil {
		return nil, err
	}
	return binding, nil
}

func decodeReconcilerExecutionBinding(data []byte) (*reconcilerExecutionBinding, error) {
	if err := rejectExecutionBindingDuplicateJSONKeys(data); err != nil {
		return nil, fmt.Errorf("validating unique JSON keys: %w", err)
	}

	var binding reconcilerExecutionBinding
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&binding); err != nil {
		return nil, fmt.Errorf("decoding binding: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, errors.New("binding contains a trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("binding contains trailing invalid data: %w", err)
	}
	canonical, err := json.Marshal(binding)
	if err != nil {
		return nil, fmt.Errorf("canonicalizing decoded binding: %w", err)
	}
	rawValue, err := decodeExecutionBindingJSONValue(data)
	if err != nil {
		return nil, fmt.Errorf("decoding raw binding shape: %w", err)
	}
	canonicalValue, err := decodeExecutionBindingJSONValue(canonical)
	if err != nil {
		return nil, fmt.Errorf("decoding canonical binding shape: %w", err)
	}
	if !reflect.DeepEqual(rawValue, canonicalValue) {
		return nil, errors.New("binding keys, required fields, or JSON value types do not exactly match the typed schema")
	}
	return &binding, nil
}

func decodeExecutionBindingJSONValue(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected trailing JSON")
	}
	return value, nil
}

func validateReconcilerExecutionBindingSemantics(binding *reconcilerExecutionBinding) error {
	if binding.SchemaVersion != 2 || binding.Kind != "reconciler-pre-g0-execution-binding" || binding.Status != "bootstrap-approved-default-inert" {
		return fmt.Errorf("unexpected binding identity: schema=%d kind=%q status=%q", binding.SchemaVersion, binding.Kind, binding.Status)
	}
	if binding.HashAlgorithm != "sha256" || binding.Canonicalization != "go-encoding-json-v2-binding-digest-omitted" {
		return fmt.Errorf("unexpected hash contract: algorithm=%q canonicalization=%q", binding.HashAlgorithm, binding.Canonicalization)
	}
	if !reflect.DeepEqual(binding.Predecessor, expectedExecutionBindingPredecessor) {
		return fmt.Errorf("predecessor binding does not match the immutable v1 candidate: %#v", binding.Predecessor)
	}
	if !reflect.DeepEqual(binding.Lineage, expectedExecutionBindingLineage) {
		return fmt.Errorf("lineage does not match the pinned source/base/bootstrap and recorded upstream delta")
	}
	if !reflect.DeepEqual(binding.ImmutableV1Files, expectedExecutionBindingImmutableFiles) {
		return fmt.Errorf("immutable v1 file set does not match the approved contract")
	}
	if !reflect.DeepEqual(binding.ConflictResolutions, expectedExecutionBindingConflicts) {
		return fmt.Errorf("conflict resolutions do not match the reviewed bootstrap")
	}
	if !reflect.DeepEqual(binding.CleanMergeRetention, expectedExecutionBindingCleanMerge) {
		return fmt.Errorf("clean-merge retention does not match the reviewed duplicate-symbol correction")
	}
	if !reflect.DeepEqual(binding.CombinedTreeCensus, expectedExecutionBindingCensus) {
		return fmt.Errorf("combined-tree census does not match the independently regenerated values")
	}
	if !reflect.DeepEqual(binding.BootstrapReview, expectedExecutionBindingBootstrapReview) {
		return fmt.Errorf("bootstrap review does not match the independently approved exact artifact")
	}
	wantGateMetadata := []struct{ Path, Role, Outcome string }{
		{"test/docsync/reconciler_execution_binding_test.go", "strict-binding-verifier", "pass"},
		{".github/workflows/ci.yml", "protected-authoritative-history-and-trust-anchor", "pass"},
		{"TESTING.md", "checked-resource-ledger-table", "pass"},
		{"internal/testpolicy/resourcecensus/census.go", "checked-resource-ledger-policy", "pass"},
		{"test/test-resources.toml", "checked-resource-ledger-manifest", "pass"},
	}
	if len(binding.GateArtifacts) != len(wantGateMetadata) {
		return fmt.Errorf("gate artifact count = %d, want %d", len(binding.GateArtifacts), len(wantGateMetadata))
	}
	for i, want := range wantGateMetadata {
		got := binding.GateArtifacts[i]
		if got.Path != want.Path || got.Role != want.Role || got.ExpectedOutcome != want.Outcome {
			return fmt.Errorf("gate artifact %d metadata = {%q %q %q}, want {%q %q %q}", i, got.Path, got.Role, got.ExpectedOutcome, want.Path, want.Role, want.Outcome)
		}
		if !isExecutionBindingLowerHex(got.BlobOID, 40) || !isExecutionBindingLowerHex(got.SHA256, 64) {
			return fmt.Errorf("gate artifact %q hashes are not full lowercase object digests", got.Path)
		}
	}
	if !reflect.DeepEqual(binding.GateAttestations, expectedExecutionBindingGateAttestations) {
		return fmt.Errorf("gate attestations do not match the independently reviewed commands and outcomes")
	}
	if !reflect.DeepEqual(binding.IntroductionContract, expectedExecutionBindingIntroduction) {
		return fmt.Errorf("introduction contract does not require the exact additive commit and independent review")
	}
	if !reflect.DeepEqual(binding.Authorization, expectedExecutionBindingAuthorization) {
		return fmt.Errorf("authorization changed or broadened beyond the exact approved bootstrap scope")
	}
	if !reflect.DeepEqual(binding.TrustBoundary, expectedExecutionBindingTrustBoundary) {
		return fmt.Errorf("trust boundary overstates repository enforcement or final tamper control")
	}
	if !isExecutionBindingLowerHex(binding.BindingDigest, 64) {
		return fmt.Errorf("binding digest %q is not a full lowercase SHA-256 digest", binding.BindingDigest)
	}
	wantDigest := binding.BindingDigest
	digestInput := *binding
	digestInput.BindingDigest = ""
	canonical, err := json.Marshal(digestInput)
	if err != nil {
		return fmt.Errorf("canonicalizing binding: %w", err)
	}
	if got := executionBindingSHA256Hex(canonical); got != wantDigest {
		return fmt.Errorf("binding digest = %s, binding has %s", got, wantDigest)
	}
	return nil
}

func validateBindingCurrentArtifacts(root string, binding *reconcilerExecutionBinding) error {
	predecessorData, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(binding.Predecessor.Path)))
	if err != nil {
		return fmt.Errorf("reading current predecessor: %w", err)
	}
	if err := validateBindingFileBytes(binding.Predecessor.Path, binding.Predecessor.BlobOID, binding.Predecessor.SHA256, predecessorData); err != nil {
		return err
	}
	var predecessor executionBindingPredecessorManifest
	if err := json.Unmarshal(predecessorData, &predecessor); err != nil {
		return fmt.Errorf("decoding predecessor candidate: %w", err)
	}
	if predecessor.CandidateDigest != binding.Predecessor.CandidateDigest {
		return fmt.Errorf("predecessor candidate digest = %s, binding has %s", predecessor.CandidateDigest, binding.Predecessor.CandidateDigest)
	}
	for _, entry := range predecessor.CurrentFiles {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entry.Path)))
		if err != nil {
			return fmt.Errorf("reading predecessor current file %s: %w", entry.Path, err)
		}
		if err := validateBindingFileBytes(entry.Path, entry.BlobOID, entry.SHA256, data); err != nil {
			return fmt.Errorf("predecessor current file: %w", err)
		}
	}
	sectionFiles := make(map[string][]byte)
	for _, section := range predecessor.Sections {
		data, ok := sectionFiles[section.Path]
		if !ok {
			data, err = os.ReadFile(filepath.Join(root, filepath.FromSlash(section.Path)))
			if err != nil {
				return fmt.Errorf("reading predecessor section file %s: %w", section.Path, err)
			}
			sectionFiles[section.Path] = data
		}
		canonical, err := extractExecutionBindingCandidateSection(string(data), section.Kind, section.Selector)
		if err != nil {
			return fmt.Errorf("extracting predecessor section %s: %w", section.ID, err)
		}
		if got := executionBindingSHA256Hex([]byte(canonical)); got != section.SHA256 {
			return fmt.Errorf("predecessor section %s sha256 = %s, manifest has %s", section.ID, got, section.SHA256)
		}
	}

	for _, entry := range binding.ImmutableV1Files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entry.Path)))
		if err != nil {
			return fmt.Errorf("reading current immutable v1 file %s: %w", entry.Path, err)
		}
		if err := validateBindingFileBytes(entry.Path, entry.BlobOID, entry.SHA256, data); err != nil {
			return err
		}
	}
	return validateBindingArtifacts(root, "", binding)
}

func validateReconcilerExecutionBindingRepository(root string, supplied []byte, binding *reconcilerExecutionBinding) error {
	if err := requireFullBindingHistory(root); err != nil {
		return err
	}

	commits := []struct {
		name string
		ref  executionBindingCommit
	}{
		{"predecessor execution base", binding.Lineage.PredecessorExecutionBase},
		{"approved execution base", binding.Lineage.ApprovedExecutionBase},
		{"candidate source", executionBindingCommit{Commit: binding.Lineage.CandidateSource.Commit, Tree: binding.Lineage.CandidateSource.Tree}},
		{"bootstrap", executionBindingCommit{Commit: binding.Lineage.Bootstrap.Commit, Tree: binding.Lineage.Bootstrap.Tree}},
		{"bootstrap merge base", binding.Lineage.Bootstrap.MergeBase},
		{"approval-observed upstream", executionBindingCommit{Commit: binding.Lineage.UnincorporatedUpstream.ApprovalObservedTip, Tree: binding.Lineage.UnincorporatedUpstream.ApprovalObservedTipTree}},
		{"binding-observed upstream", executionBindingCommit{Commit: binding.Lineage.UnincorporatedUpstream.BindingObservedTip, Tree: binding.Lineage.UnincorporatedUpstream.BindingObservedTipTree}},
	}
	for _, entry := range commits {
		if err := validateExactBindingCommit(root, entry.name, entry.ref); err != nil {
			return err
		}
	}

	if err := validateBindingCommitDelta(root, "execution-base delta", binding.Lineage.ExecutionBaseDelta.FromCommit, binding.Lineage.ExecutionBaseDelta.ToCommit, binding.Lineage.ExecutionBaseDelta.CommitCount); err != nil {
		return err
	}
	if err := validateBindingCommitDelta(root, "unincorporated upstream delta", binding.Lineage.ApprovedExecutionBase.Commit, binding.Lineage.UnincorporatedUpstream.BindingObservedTip, binding.Lineage.UnincorporatedUpstream.CommitsAfterApprovedBase); err != nil {
		return err
	}
	if got, err := bindingGitOutput(root, "merge-base", binding.Lineage.UnincorporatedUpstream.ApprovalObservedTip, binding.Lineage.UnincorporatedUpstream.BindingObservedTip); err != nil {
		return err
	} else if got != binding.Lineage.UnincorporatedUpstream.ApprovalObservedTip {
		return fmt.Errorf("approval-observed upstream %s is not an ancestor of binding-observed upstream %s", binding.Lineage.UnincorporatedUpstream.ApprovalObservedTip, binding.Lineage.UnincorporatedUpstream.BindingObservedTip)
	}

	bootstrapCommit, err := readRawBindingCommit(root, binding.Lineage.Bootstrap.Commit)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(bootstrapCommit.Parents, binding.Lineage.Bootstrap.OrderedParents) {
		return fmt.Errorf("bootstrap parents = %v, want exact ordered parents %v", bootstrapCommit.Parents, binding.Lineage.Bootstrap.OrderedParents)
	}
	if got, err := bindingGitOutput(root, "merge-base", binding.Lineage.ApprovedExecutionBase.Commit, binding.Lineage.CandidateSource.Commit); err != nil {
		return err
	} else if got != binding.Lineage.Bootstrap.MergeBase.Commit {
		return fmt.Errorf("bootstrap merge base = %s, want %s", got, binding.Lineage.Bootstrap.MergeBase.Commit)
	}

	if err := validateBindingPredecessor(root, binding); err != nil {
		return err
	}
	if err := validateBindingConflictObjects(root, binding); err != nil {
		return err
	}

	introduction, err := deriveBindingIntroduction(root, supplied, binding)
	if err != nil {
		return err
	}
	if introduction != "" {
		for _, entry := range binding.ImmutableV1Files {
			data, err := bindingGitBytes(root, "show", introduction+":"+entry.Path)
			if err != nil {
				return fmt.Errorf("reading immutable v1 file %s at binding introduction: %w", entry.Path, err)
			}
			if err := validateBindingPathUnchanged(root, introduction, entry.Path, data); err != nil {
				return err
			}
		}
	}
	if err := validateBindingArtifacts(root, introduction, binding); err != nil {
		return err
	}
	return validateBindingCIChange(root, introduction, binding)
}

func requireFullBindingHistory(root string) error {
	got, err := bindingGitOutput(root, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return fmt.Errorf("checking repository history depth: %w", err)
	}
	if got != "false" {
		return fmt.Errorf("%w; git reported %q", errBindingShallowRepository, got)
	}
	replacements, err := bindingGitOutput(root, "replace", "-l")
	if err != nil {
		return fmt.Errorf("checking Git replacement refs: %w", err)
	}
	if replacements != "" {
		return fmt.Errorf("execution binding refuses Git replacement refs: %s", replacements)
	}
	commonDir, err := bindingGitOutput(root, "rev-parse", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("resolving Git common directory: %w", err)
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(root, commonDir)
	}
	graftsPath := filepath.Join(filepath.Clean(commonDir), "info", "grafts")
	if _, err := os.Stat(graftsPath); err == nil {
		return fmt.Errorf("execution binding refuses legacy Git grafts at %s", graftsPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking legacy Git grafts at %s: %w", graftsPath, err)
	}
	return nil
}

func validateExactBindingCommit(root, name string, ref executionBindingCommit) error {
	if !isExecutionBindingLowerHex(ref.Commit, 40) || !isExecutionBindingLowerHex(ref.Tree, 40) {
		return fmt.Errorf("%s has malformed commit/tree identity", name)
	}
	commit, err := bindingGitOutput(root, "rev-parse", "--verify", ref.Commit+"^{commit}")
	if err != nil {
		return fmt.Errorf("resolving %s commit %s: %w", name, ref.Commit, err)
	}
	if commit != ref.Commit {
		return fmt.Errorf("%s resolves to commit %s, want exact object %s", name, commit, ref.Commit)
	}
	raw, err := readRawBindingCommit(root, ref.Commit)
	if err != nil {
		return fmt.Errorf("reading raw %s commit %s: %w", name, ref.Commit, err)
	}
	if raw.Tree != ref.Tree {
		return fmt.Errorf("%s tree = %s, want %s", name, raw.Tree, ref.Tree)
	}
	return nil
}

func validateBindingCommitDelta(root, name, from, to string, wantCount int) error {
	mergeBase, err := bindingGitOutput(root, "merge-base", from, to)
	if err != nil {
		return fmt.Errorf("resolving %s merge base: %w", name, err)
	}
	if mergeBase != from {
		return fmt.Errorf("%s is not a forward ancestry delta: merge base = %s, want %s", name, mergeBase, from)
	}
	countText, err := bindingGitOutput(root, "rev-list", "--count", from+".."+to)
	if err != nil {
		return fmt.Errorf("counting %s: %w", name, err)
	}
	count, err := strconv.Atoi(countText)
	if err != nil {
		return fmt.Errorf("parsing %s count %q: %w", name, countText, err)
	}
	if count != wantCount {
		return fmt.Errorf("%s commit count = %d, want %d", name, count, wantCount)
	}
	return nil
}

func validateBindingPredecessor(root string, binding *reconcilerExecutionBinding) error {
	for _, commit := range []string{binding.Lineage.CandidateSource.Commit, binding.Lineage.Bootstrap.Commit} {
		data, err := bindingGitBytes(root, "show", commit+":"+binding.Predecessor.Path)
		if err != nil {
			return fmt.Errorf("reading predecessor at %s: %w", commit, err)
		}
		if err := validateBindingFileBytes(binding.Predecessor.Path, binding.Predecessor.BlobOID, binding.Predecessor.SHA256, data); err != nil {
			return fmt.Errorf("predecessor at %s: %w", commit, err)
		}
	}
	current, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(binding.Predecessor.Path)))
	if err != nil {
		return fmt.Errorf("reading current predecessor: %w", err)
	}
	if err := validateBindingFileBytes(binding.Predecessor.Path, binding.Predecessor.BlobOID, binding.Predecessor.SHA256, current); err != nil {
		return err
	}
	var predecessor executionBindingPredecessorManifest
	if err := json.Unmarshal(current, &predecessor); err != nil {
		return fmt.Errorf("decoding predecessor candidate: %w", err)
	}
	if predecessor.CandidateDigest != binding.Predecessor.CandidateDigest {
		return fmt.Errorf("predecessor candidate digest = %s, binding has %s", predecessor.CandidateDigest, binding.Predecessor.CandidateDigest)
	}

	for _, entry := range binding.ImmutableV1Files {
		for _, commit := range []string{binding.Lineage.CandidateSource.Commit, binding.Lineage.Bootstrap.Commit} {
			data, err := bindingGitBytes(root, "show", commit+":"+entry.Path)
			if err != nil {
				return fmt.Errorf("reading immutable v1 file %s at %s: %w", entry.Path, commit, err)
			}
			if err := validateBindingFileBytes(entry.Path, entry.BlobOID, entry.SHA256, data); err != nil {
				return fmt.Errorf("immutable v1 file at %s: %w", commit, err)
			}
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entry.Path)))
		if err != nil {
			return fmt.Errorf("reading current immutable v1 file %s: %w", entry.Path, err)
		}
		if err := validateBindingFileBytes(entry.Path, entry.BlobOID, entry.SHA256, data); err != nil {
			return err
		}
	}
	return nil
}

func validateBindingConflictObjects(root string, binding *reconcilerExecutionBinding) error {
	mergeBase := binding.Lineage.Bootstrap.MergeBase.Commit
	parent1 := binding.Lineage.Bootstrap.OrderedParents[0]
	parent2 := binding.Lineage.Bootstrap.OrderedParents[1]
	bootstrap := binding.Lineage.Bootstrap.Commit
	for _, entry := range binding.ConflictResolutions {
		checks := []struct{ commit, want, label string }{
			{mergeBase, entry.AncestorBlobOID, "ancestor"},
			{parent1, entry.Parent1BlobOID, "parent 1"},
			{parent2, entry.Parent2BlobOID, "parent 2"},
			{bootstrap, entry.ResultBlobOID, "result"},
		}
		for _, check := range checks {
			got, err := bindingGitOutput(root, "rev-parse", "--verify", check.commit+":"+entry.Path)
			if err != nil {
				return fmt.Errorf("resolving %s conflict blob for %s: %w", check.label, entry.Path, err)
			}
			if got != check.want {
				return fmt.Errorf("%s conflict blob for %s = %s, want %s", check.label, entry.Path, got, check.want)
			}
		}
		result, err := bindingGitBytes(root, "show", bootstrap+":"+entry.Path)
		if err != nil {
			return fmt.Errorf("reading conflict result %s: %w", entry.Path, err)
		}
		if err := validateBindingFileBytes(entry.Path, entry.ResultBlobOID, entry.SHA256, result); err != nil {
			return err
		}
	}

	entry := binding.CleanMergeRetention
	checks := []struct{ commit, want, label string }{
		{mergeBase, entry.AncestorBlobOID, "ancestor"},
		{parent1, entry.Parent1BlobOID, "parent 1"},
		{parent2, entry.Parent2BlobOID, "parent 2"},
		{bootstrap, entry.ResultBlobOID, "result"},
	}
	for _, check := range checks {
		got, err := bindingGitOutput(root, "rev-parse", "--verify", check.commit+":"+entry.Path)
		if err != nil {
			return fmt.Errorf("resolving clean-merge %s blob for %s: %w", check.label, entry.Path, err)
		}
		if got != check.want {
			return fmt.Errorf("clean-merge %s blob for %s = %s, want %s", check.label, entry.Path, got, check.want)
		}
	}
	result, err := bindingGitBytes(root, "show", bootstrap+":"+entry.Path)
	if err != nil {
		return fmt.Errorf("reading clean-merge retention %s: %w", entry.Path, err)
	}
	return validateBindingFileBytes(entry.Path, entry.ResultBlobOID, entry.SHA256, result)
}

func deriveBindingIntroduction(root string, supplied []byte, binding *reconcilerExecutionBinding) (string, error) {
	present, err := bindingGitOutput(root, "ls-tree", "-r", "--name-only", "HEAD", "--", reconcilerExecutionBindingPath)
	if err != nil {
		return "", fmt.Errorf("checking binding presence at HEAD: %w", err)
	}
	if present == "" {
		head, err := bindingGitOutput(root, "rev-parse", "HEAD")
		if err != nil {
			return "", err
		}
		if head != binding.IntroductionContract.ParentCommit {
			return "", fmt.Errorf("pending binding may exist only on exact parent %s; HEAD is %s", binding.IntroductionContract.ParentCommit, head)
		}
		return "", nil
	}
	if present != reconcilerExecutionBindingPath {
		return "", fmt.Errorf("unexpected binding path at HEAD: %q", present)
	}

	introductions, err := bindingIntroductionCommits(root, reconcilerExecutionBindingPath)
	if err != nil {
		return "", fmt.Errorf("deriving binding introduction: %w", err)
	}
	if len(introductions) != 1 {
		return "", fmt.Errorf("binding introduction count = %d, want exactly 1", len(introductions))
	}
	introduction := introductions[0]
	rawIntroduction, err := readRawBindingCommit(root, introduction)
	if err != nil {
		return "", err
	}
	wantParents := []string{binding.IntroductionContract.ParentCommit}
	if !reflect.DeepEqual(rawIntroduction.Parents, wantParents) {
		return "", fmt.Errorf("binding introduction parents = %v, want %v", rawIntroduction.Parents, wantParents)
	}

	nameStatus, err := bindingGitOutput(root, "diff-tree", "--no-commit-id", "--name-status", "-r", introduction)
	if err != nil {
		return "", fmt.Errorf("reading binding introduction changed paths: %w", err)
	}
	gotPaths, err := parseBindingChangedPaths(nameStatus)
	if err != nil {
		return "", err
	}
	wantPaths := append([]executionBindingChangedPath(nil), binding.IntroductionContract.ChangedPaths...)
	sort.Slice(wantPaths, func(i, j int) bool { return wantPaths[i].Path < wantPaths[j].Path })
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		return "", fmt.Errorf("binding introduction changed paths = %#v, want %#v", gotPaths, wantPaths)
	}

	introduced, err := bindingGitBytes(root, "show", introduction+":"+reconcilerExecutionBindingPath)
	if err != nil {
		return "", fmt.Errorf("reading introduced binding: %w", err)
	}
	if !bytes.Equal(introduced, supplied) {
		return "", errors.New("supplied binding bytes differ from the unique introduction blob")
	}
	headBytes, err := bindingGitBytes(root, "show", "HEAD:"+reconcilerExecutionBindingPath)
	if err != nil {
		return "", fmt.Errorf("reading binding at HEAD: %w", err)
	}
	if !bytes.Equal(headBytes, introduced) {
		return "", errors.New("binding at HEAD differs from the unique introduction blob")
	}
	if err := validateBindingPathUnchanged(root, introduction, reconcilerExecutionBindingPath, introduced); err != nil {
		return "", err
	}
	return introduction, nil
}

func validateBindingArtifacts(root, introduction string, binding *reconcilerExecutionBinding) error {
	review := binding.BootstrapReview
	reviewData, err := validateBindingArtifact(root, introduction, review.Path, review.BlobOID, review.SHA256)
	if err != nil {
		return err
	}
	if err := validateBootstrapReviewContent(reviewData); err != nil {
		return err
	}
	for _, artifact := range binding.GateArtifacts {
		if _, err := validateBindingArtifact(root, introduction, artifact.Path, artifact.BlobOID, artifact.SHA256); err != nil {
			return err
		}
	}
	return nil
}

func validateBindingArtifact(root, introduction, path, blobOID, sha256 string) ([]byte, error) {
	current, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return nil, fmt.Errorf("reading gate artifact %s: %w", path, err)
	}
	if err := validateBindingFileBytes(path, blobOID, sha256, current); err != nil {
		return nil, err
	}
	if introduction == "" {
		return current, nil
	}
	introduced, err := bindingGitBytes(root, "show", introduction+":"+path)
	if err != nil {
		return nil, fmt.Errorf("reading introduced gate artifact %s: %w", path, err)
	}
	if !bytes.Equal(introduced, current) {
		return nil, fmt.Errorf("gate artifact %s differs from its introduction blob", path)
	}
	head, err := bindingGitBytes(root, "show", "HEAD:"+path)
	if err != nil {
		return nil, fmt.Errorf("reading gate artifact %s at HEAD: %w", path, err)
	}
	if !bytes.Equal(head, introduced) {
		return nil, fmt.Errorf("gate artifact %s at HEAD differs from its introduction blob", path)
	}
	if err := validateBindingPathUnchanged(root, introduction, path, introduced); err != nil {
		return nil, err
	}
	return current, nil
}

func validateBindingPathUnchanged(root, introduction, path string, introduced []byte) error {
	wantEntry := "100644 blob " + executionBindingGitBlobOID(introduced) + "\t" + path
	entry, err := bindingGitOutput(root, "ls-tree", introduction, "--", path)
	if err != nil {
		return err
	}
	if entry != wantEntry {
		return fmt.Errorf("introduced tree entry for %s = %q, want %q", path, entry, wantEntry)
	}
	rawIntroduction, err := readRawBindingCommit(root, introduction)
	if err != nil {
		return fmt.Errorf("resolving binding introduction parent for %s: %w", path, err)
	}
	if len(rawIntroduction.Parents) != 1 {
		return fmt.Errorf("binding introduction parent count for %s = %d, want 1", path, len(rawIntroduction.Parents))
	}
	parent := rawIntroduction.Parents[0]
	// Scan every path-bearing commit reachable after the introduction parent,
	// including merged side branches that forked before the introduction.
	candidates, err := bindingGitFields(root, "rev-list", "--full-history", parent+"..HEAD", "--", path)
	if err != nil {
		return fmt.Errorf("checking post-introduction history for %s: %w", path, err)
	}
	for _, commit := range candidates {
		got, err := bindingGitOutput(root, "ls-tree", commit, "--", path)
		if err != nil {
			return fmt.Errorf("reading %s at post-introduction commit %s: %w", path, commit, err)
		}
		if got != wantEntry {
			return fmt.Errorf("post-introduction mutation of %s at %s: tree entry %q, want %q", path, commit, got, wantEntry)
		}
	}
	return nil
}

func validateBindingCIChange(root, introduction string, binding *reconcilerExecutionBinding) error {
	verifierBlob := ""
	verifierSHA256 := ""
	for _, artifact := range binding.GateArtifacts {
		if artifact.Path == "test/docsync/reconciler_execution_binding_test.go" {
			verifierBlob = artifact.BlobOID
			verifierSHA256 = artifact.SHA256
			break
		}
	}
	if verifierBlob == "" || verifierSHA256 == "" {
		return errors.New("binding verifier gate artifact is missing")
	}

	for _, workflow := range binding.IntroductionContract.CIHistory.FullHistoryWorkflows {
		base, err := bindingGitBytes(root, "show", binding.IntroductionContract.ParentCommit+":"+workflow.Path)
		if err != nil {
			return fmt.Errorf("reading bootstrap workflow %s: %w", workflow.Path, err)
		}
		want, err := workflowWithFullHistory(base, workflow.Jobs)
		if err != nil {
			return fmt.Errorf("building expected full-history workflow %s: %w", workflow.Path, err)
		}
		if workflow.Path == binding.IntroductionContract.CIHistory.AuthoritativeWorkflow {
			want, err = workflowWithBindingTrustAnchor(want, verifierBlob, verifierSHA256)
			if err != nil {
				return err
			}
		}
		current, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(workflow.Path)))
		if err != nil {
			return fmt.Errorf("reading current workflow %s: %w", workflow.Path, err)
		}
		if !bytes.Equal(current, want) {
			return fmt.Errorf("workflow %s differs from its exact authorized history/trust-anchor change", workflow.Path)
		}
		if introduction != "" {
			introduced, err := bindingGitBytes(root, "show", introduction+":"+workflow.Path)
			if err != nil {
				return fmt.Errorf("reading introduced workflow %s: %w", workflow.Path, err)
			}
			if !bytes.Equal(introduced, want) {
				return fmt.Errorf("introduced workflow %s differs from the exact authorized history/trust-anchor change", workflow.Path)
			}
		}
	}
	return nil
}

func workflowWithFullHistory(base []byte, jobs []string) ([]byte, error) {
	const checkout = "      - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6\n"
	trailingNewline := bytes.HasSuffix(base, []byte("\n"))
	lines := strings.Split(strings.TrimSuffix(string(base), "\n"), "\n")
	checkoutLine := strings.TrimSuffix(checkout, "\n")
	for _, job := range jobs {
		start := -1
		for i, line := range lines {
			if line == "  "+job+":" {
				start = i
				break
			}
		}
		if start < 0 {
			return nil, fmt.Errorf("job %q not found", job)
		}
		end := len(lines)
		for i := start + 1; i < len(lines); i++ {
			line := lines[i]
			if strings.HasPrefix(line, "  ") && len(line) > 2 && line[2] != ' ' && line[2] != '#' && strings.HasSuffix(line, ":") {
				end = i
				break
			}
		}
		checkoutIndex := -1
		for i := start + 1; i < end; i++ {
			if lines[i] == checkoutLine {
				if checkoutIndex >= 0 {
					return nil, fmt.Errorf("job %q has multiple repository checkouts", job)
				}
				checkoutIndex = i
			}
			if strings.TrimSpace(lines[i]) == "fetch-depth: 0" {
				return nil, fmt.Errorf("job %q already has a full-history checkout", job)
			}
		}
		if checkoutIndex < 0 {
			return nil, fmt.Errorf("job %q has no exact repository checkout", job)
		}
		if checkoutIndex+1 < len(lines) && lines[checkoutIndex+1] == "        with:" {
			lines = insertBindingWorkflowLines(lines, checkoutIndex+2, "          fetch-depth: 0")
		} else {
			lines = insertBindingWorkflowLines(lines, checkoutIndex+1, "        with:", "          fetch-depth: 0")
		}
	}
	result := strings.Join(lines, "\n")
	if trailingNewline {
		result += "\n"
	}
	return []byte(result), nil
}

func insertBindingWorkflowLines(lines []string, index int, additions ...string) []string {
	result := make([]string, 0, len(lines)+len(additions))
	result = append(result, lines[:index]...)
	result = append(result, additions...)
	result = append(result, lines[index:]...)
	return result
}

func workflowWithBindingTrustAnchor(workflow []byte, verifierBlob, verifierSHA256 string) ([]byte, error) {
	const setupBoundary = `      - uses: actions/setup-go@4a3601121dd01d1626a1e23e37211e3254c1c06c # v6
        with:
          go-version-file: go.mod
      - name: go.mod replace guard
`
	if strings.Count(string(workflow), setupBoundary) != 1 {
		return nil, errors.New("authoritative workflow does not have the exact setup-go to first-repository-command boundary")
	}
	anchor := fmt.Sprintf(`      - name: Reconciler execution binding trust anchor
        run: |
          set -euo pipefail
          test "$(git rev-parse --is-shallow-repository)" = "false"
          test "$(git hash-object --no-filters test/docsync/reconciler_execution_binding_test.go)" = "%s"
          test "$(sha256sum test/docsync/reconciler_execution_binding_test.go | awk '{print $1}')" = "%s"
          GC_BINDING_AUTHORITATIVE=1 GOWORK=off GOFLAGS= GOENV=off GOTOOLCHAIN=local CGO_ENABLED=0 \
            go test -mod=readonly -count=1 \
              test/docsync/reconciler_execution_binding_test.go \
              -run '^(TestReconciler.*|TestScrubBindingGitEnvironment)$'
`, verifierBlob, verifierSHA256)
	replacement := strings.TrimSuffix(setupBoundary, "      - name: go.mod replace guard\n") + anchor + "      - name: go.mod replace guard\n"
	return []byte(strings.Replace(string(workflow), setupBoundary, replacement, 1)), nil
}

func validateBootstrapReviewContent(data []byte) error {
	required := []string{
		"| Verdict | **APPROVE** |",
		"614a8ebd62cf230de226213f302ed3002dddea61",
		"2c1c9d4a2aeaf8188793a26554ec7ea5f9b7cc48",
		"d36a8ccadf63c9c782b799e2a02ffbfce12c7dd4",
		"8f46e6ed3930f32e2ec59ac0a70b7328558bfa41",
		"62ac13ec9fc3897b01269ee377a7c84fe3e65f3a",
		"a5b3e4fb4701627ffaa65711df26e0161dedbe8d",
		"cf2ad511ce76cc0630edff2ec0b9f46e952db487",
		"5450456ff422c2b0374245dd89d71b2482a4ecc0",
		"a7d6303916ac4a445839de2834f88addcd371e42",
		"492 / 137",
		"438 / 153",
		"4173 / 185",
		"4167 / 185",
		"No verification or cleanup touched the default tmux server.",
		"does not ratify operational G0",
		"does not authorize a production owner cutover, provider effects, runtime action concurrency, or schema mutation",
	}
	text := strings.Join(strings.Fields(string(data)), " ")
	for _, value := range required {
		if !strings.Contains(text, value) {
			return fmt.Errorf("bootstrap review is missing required evidence %q", value)
		}
	}
	return nil
}

func validateBindingFileBytes(path, blobOID, sha256 string, data []byte) error {
	if got := executionBindingGitBlobOID(data); got != blobOID {
		return fmt.Errorf("%s blob OID = %s, binding has %s", path, got, blobOID)
	}
	if got := executionBindingSHA256Hex(data); got != sha256 {
		return fmt.Errorf("%s sha256 = %s, binding has %s", path, got, sha256)
	}
	return nil
}

var executionBindingInvariantStartRE = regexp.MustCompile(`^[0-9]+\. \*\*INV-[0-9]+\*\*`)

func extractExecutionBindingCandidateSection(content, kind, selector string) (string, error) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	start := -1
	for index, line := range lines {
		if strings.HasPrefix(line, selector) {
			if start >= 0 {
				return "", fmt.Errorf("selector %q is ambiguous", selector)
			}
			start = index
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
		for index := start + 1; index < len(lines); index++ {
			candidateLevel := strings.IndexByte(lines[index], ' ')
			if candidateLevel >= 1 && candidateLevel <= level && strings.Trim(lines[index][:candidateLevel], "#") == "" {
				end = index
				break
			}
		}
	case "numbered-invariant":
		for index := start + 1; index < len(lines); index++ {
			if executionBindingInvariantStartRE.MatchString(lines[index]) {
				end = index
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

func parseBindingChangedPaths(output string) ([]executionBindingChangedPath, error) {
	if strings.TrimSpace(output) == "" {
		return nil, errors.New("binding introduction changed no paths")
	}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	result := make([]executionBindingChangedPath, 0, len(lines))
	for _, line := range lines {
		parts := strings.Split(line, "\t")
		if len(parts) != 2 || (parts[0] != "A" && parts[0] != "M") || parts[1] == "" {
			return nil, fmt.Errorf("unexpected binding introduction name-status line %q", line)
		}
		result = append(result, executionBindingChangedPath{Status: parts[0], Path: parts[1]})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	for i := 1; i < len(result); i++ {
		if result[i-1].Path == result[i].Path {
			return nil, fmt.Errorf("duplicate binding introduction path %q", result[i].Path)
		}
	}
	return result, nil
}

func readRawBindingCommit(root, oid string) (rawBindingCommit, error) {
	if !isExecutionBindingLowerHex(oid, 40) {
		return rawBindingCommit{}, fmt.Errorf("malformed commit object ID %q", oid)
	}
	data, err := bindingGitBytes(root, "cat-file", "commit", oid)
	if err != nil {
		return rawBindingCommit{}, err
	}
	var result rawBindingCommit
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "tree ") {
			if result.Tree != "" {
				return rawBindingCommit{}, fmt.Errorf("commit %s has duplicate tree headers", oid)
			}
			result.Tree = strings.TrimPrefix(line, "tree ")
		}
		if strings.HasPrefix(line, "parent ") {
			result.Parents = append(result.Parents, strings.TrimPrefix(line, "parent "))
		}
	}
	if !isExecutionBindingLowerHex(result.Tree, 40) {
		return rawBindingCommit{}, fmt.Errorf("commit %s has malformed or missing raw tree %q", oid, result.Tree)
	}
	for _, parent := range result.Parents {
		if !isExecutionBindingLowerHex(parent, 40) {
			return rawBindingCommit{}, fmt.Errorf("commit %s has malformed raw parent %q", oid, parent)
		}
	}
	return result, nil
}

func bindingIntroductionCommits(root, path string) ([]string, error) {
	return bindingGitFields(root, "log", "--format=%H", "--full-history", "--diff-filter=A", "HEAD", "--", path)
}

func bindingGitFields(root string, args ...string) ([]string, error) {
	output, err := bindingGitOutput(root, args...)
	if err != nil {
		return nil, err
	}
	if output == "" {
		return []string{}, nil
	}
	return strings.Fields(output), nil
}

func bindingGitOutput(root string, args ...string) (string, error) {
	data, err := bindingGitBytes(root, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func bindingGitBytes(root string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bindingGitCommandTimeout)
	defer cancel()
	cmd := newExecutionBindingGitCommand(ctx, root, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("git %s timed out after %s", strings.Join(args, " "), bindingGitCommandTimeout)
		}
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, detail)
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return data, nil
}

func scrubBindingGitEnvironment(input []string) []string {
	result := make([]string, 0, len(input)+6)
	for _, value := range input {
		if !strings.HasPrefix(value, "GIT_") {
			result = append(result, value)
		}
	}
	return append(result,
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_OPTIONAL_LOCKS=0",
	)
}

func newExecutionBindingGitCommand(ctx context.Context, root string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	cmd.Env = scrubBindingGitEnvironment(os.Environ())
	return cmd
}

func rejectExecutionBindingDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanUniqueExecutionBindingJSONValue(decoder, "$"); err != nil {
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

func scanUniqueExecutionBindingJSONValue(decoder *json.Decoder, path string) error {
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
			if err := scanUniqueExecutionBindingJSONValue(decoder, path+"."+key); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("closing object %s: %w", path, err)
		}
		if end != json.Delim('}') {
			return fmt.Errorf("closing object %s: got %q", path, end)
		}
	case '[':
		for index := 0; decoder.More(); index++ {
			if err := scanUniqueExecutionBindingJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("closing array %s: %w", path, err)
		}
		if end != json.Delim(']') {
			return fmt.Errorf("closing array %s: got %q", path, end)
		}
	default:
		return fmt.Errorf("unexpected delimiter %q at %s", delim, path)
	}
	return nil
}

func executionBindingSHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func executionBindingGitBlobOID(data []byte) string {
	h := sha1.New() //nolint:gosec // This reproduces Git's object ID, not a security decision.
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(data))
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func isExecutionBindingLowerHex(value string, length int) bool {
	if len(value) != length || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
