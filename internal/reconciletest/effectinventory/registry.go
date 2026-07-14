// Package effectinventory owns the mechanically checked registry of production
// reconciler effects. The registry describes current ownership; it does not
// execute or authorize effects.
package effectinventory

import (
	"crypto/sha256"
	"fmt"
	"go/token"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// EffectKind classifies a physical side-effect boundary.
type EffectKind string

// EffectKind values enumerate the physical boundaries in scope.
const (
	KindStoreMutation    EffectKind = "authoritative-store-mutation"
	KindProviderMutation EffectKind = "provider-mutation"
	KindProcessMutation  EffectKind = "process-mutation"
	KindEventEmission    EffectKind = "event-emission"
	KindWakeSource       EffectKind = "wake-source"
)

// StoreDomain gives every authoritative store mutation a specific domain.
type StoreDomain string

// StoreDomain values enumerate authoritative store ownership domains.
const (
	StoreDomainSessionLifecycle StoreDomain = "session-lifecycle"
	StoreDomainWaitIntent       StoreDomain = "wait-intent"
	StoreDomainNudgeIntent      StoreDomain = "nudge-intent"
	StoreDomainRouteRecovery    StoreDomain = "route-recovery"
	StoreDomainPoolRouting      StoreDomain = "pool-routing"
	StoreDomainControlDispatch  StoreDomain = "control-dispatch"
	StoreDomainOrderDispatch    StoreDomain = "order-dispatch"
	StoreDomainMaintenance      StoreDomain = "maintenance"
)

// BuildProfileID identifies an analyzed source-selection equivalence class.
// The analyzer uses one representative architecture per GOOS/tag class and
// rejects every scoped production file absent from their union; a future
// architecture-specific file therefore forces a new profile instead of hiding.
type BuildProfileID string

// BuildProfileID values enumerate current source-selection classes.
const (
	BuildDarwinDefault  BuildProfileID = "darwin/default"
	BuildDarwinNative   BuildProfileID = "darwin/gascity_native_beads"
	BuildLinuxDefault   BuildProfileID = "linux/default"
	BuildLinuxNative    BuildProfileID = "linux/gascity_native_beads"
	BuildWindowsCompile BuildProfileID = "windows/compile-only"
)

// ObjectMatchKind controls how a boundary object matches typed calls.
type ObjectMatchKind string

// ObjectMatchKind values define typed boundary-dispatch matching.
const (
	ObjectMatchExact                 ObjectMatchKind = "exact"
	ObjectMatchInterfaceImplementors ObjectMatchKind = "interface-method-and-implementors"
	ObjectMatchChannel               ObjectMatchKind = "channel-object"
)

// OperationKind identifies a call or channel operation in source and SSA.
type OperationKind string

// OperationKind values enumerate source and SSA operation forms.
const (
	OperationCall           OperationKind = "call"
	OperationGo             OperationKind = "go"
	OperationDefer          OperationKind = "defer"
	OperationChannelSend    OperationKind = "channel-send"
	OperationChannelReceive OperationKind = "channel-receive"
	OperationSelectSend     OperationKind = "select-send"
	OperationSelectReceive  OperationKind = "select-receive"
)

// ExecutingProcess identifies where an ownership route actually runs.
type ExecutingProcess string

// ExecutingProcess values enumerate current process ownership contexts.
const (
	ProcessController      ExecutingProcess = "controller"
	ProcessAPIInController ExecutingProcess = "api-in-controller"
	ProcessForegroundCLI   ExecutingProcess = "foreground-cli"
	ProcessSidecarPoller   ExecutingProcess = "sidecar-poller"
	ProcessProviderChild   ExecutingProcess = "provider-child"
)

// ActionFamily is an inventory-local migration unit. The first fourteen values
// are the authoritative P7.13 session-family census.
type ActionFamily string

// ActionFamily values enumerate the inventory's migration units.
const (
	FamilyStatusHeal                ActionFamily = "status-heal"
	FamilyIdentityHealRetirement    ActionFamily = "identity-heal-retirement"
	FamilyPrimePromptDelivery       ActionFamily = "prime-prompt-delivery"
	FamilyInterruptStopTurn         ActionFamily = "interrupt-stop-turn"
	FamilyStartInitiation           ActionFamily = "start-initiation"
	FamilyStartConfirmationAdoption ActionFamily = "start-confirmation-adoption"
	FamilyDrainBeginCancel          ActionFamily = "drain-begin-cancel"
	FamilyDrainAckCompletion        ActionFamily = "drain-ack-completion"
	FamilyTimersWake                ActionFamily = "timers-wake"
	FamilyLiveConfig                ActionFamily = "live-config"
	FamilyRestartGeneration         ActionFamily = "restart-required-generation"
	FamilyProviderSwap              ActionFamily = "provider-swap"
	FamilyStop                      ActionFamily = "stop"
	FamilyCloseWorkRelease          ActionFamily = "close-work-release"
	FamilyNudge                     ActionFamily = "nudge"
	FamilyWaitIntent                ActionFamily = "wait-intent"
	FamilyPoolScale                 ActionFamily = "pool-scale"
	FamilyRouteRecovery             ActionFamily = "route-recovery"
	FamilyControlDispatch           ActionFamily = "control-dispatch"
	FamilyOrderDispatch             ActionFamily = "order-dispatch"
	FamilyMaintenance               ActionFamily = "maintenance"
	FamilyObservation               ActionFamily = "observation"
	FamilyControllerWake            ActionFamily = "controller-wake"
	FamilyRuntimeProvision          ActionFamily = "runtime-provision"
	FamilyRuntimeLaunch             ActionFamily = "runtime-launch"
	FamilyRuntimeTeardown           ActionFamily = "runtime-teardown"
	FamilyServerTeardown            ActionFamily = "server-teardown"
	FamilyProcessSignal             ActionFamily = "process-signal"
	FamilyOperatorAttach            ActionFamily = "operator-terminal-attach"
)

// AccessPath distinguishes canonical boundaries from legacy bypasses.
type AccessPath string

// AccessPath values distinguish canonical seams from legacy bypasses.
const (
	AccessWorkerBoundary        AccessPath = "worker-boundary"
	AccessSessionStoreFrontDoor AccessPath = "session-store-front-door"
	AccessManagerBypass         AccessPath = "manager-bypass"
	AccessProviderBypass        AccessPath = "provider-bypass"
	AccessRawStoreBypass        AccessPath = "raw-store-bypass"
	AccessStoreAdapter          AccessPath = "store-adapter"
	AccessProviderNative        AccessPath = "provider-native-internal"
	AccessProcessBoundary       AccessPath = "process-boundary"
	AccessDirectProcessBypass   AccessPath = "direct-process-bypass"
	AccessDirectEvent           AccessPath = "direct-event"
	AccessDirectWake            AccessPath = "direct-wake"
)

// TargetKind identifies the identity class acted upon by an effect.
type TargetKind string

// TargetKind values enumerate affected identity classes.
const (
	TargetDurableRecord         TargetKind = "durable-record"
	TargetDurableGraph          TargetKind = "durable-graph"
	TargetDurableDependencyEdge TargetKind = "durable-dependency-edge"
	TargetDurableTransaction    TargetKind = "durable-transaction"
	TargetSessionIdentity       TargetKind = "session-identity"
	TargetRuntimeIdentity       TargetKind = "runtime-identity"
	TargetProcessIdentity       TargetKind = "process-identity"
	TargetProviderServer        TargetKind = "provider-server"
	TargetEventLog              TargetKind = "event-log"
	TargetControllerChannel     TargetKind = "controller-channel"
	TargetOperatorTerminal      TargetKind = "operator-terminal"
)

// TargetCardinality records how many independently identified targets one
// physical boundary crossing may affect.
type TargetCardinality string

// TargetCardinality values distinguish one target, an explicit set, a graph
// whose target count is plan-defined, and effects selected inside a callback.
const (
	TargetCardinalityOne      TargetCardinality = "one"
	TargetCardinalitySet      TargetCardinality = "set"
	TargetCardinalityPlan     TargetCardinality = "plan"
	TargetCardinalityCallback TargetCardinality = "callback-defined"
)

// TargetIdentityKind records how target identity comes into existence.
type TargetIdentityKind string

// TargetIdentityKind values distinguish pre-existing, generated, composite,
// singleton, append-only, and callback-selected identities.
const (
	TargetIdentityExisting          TargetIdentityKind = "existing"
	TargetIdentityGenerated         TargetIdentityKind = "generated"
	TargetIdentitySymbolicGenerated TargetIdentityKind = "symbolic-to-generated"
	TargetIdentityComposite         TargetIdentityKind = "composite"
	TargetIdentitySingleton         TargetIdentityKind = "singleton"
	TargetIdentityAppendRecord      TargetIdentityKind = "append-record"
	TargetIdentityCallbackEffects   TargetIdentityKind = "callback-effects"
)

// TargetSignatureKind names the bounded argument/result shape of an effect.
type TargetSignatureKind string

// TargetSignatureKind values cover every current reconciler effect shape
// without relying on prose to distinguish generated IDs, collections, or
// composite identities.
const (
	TargetSignatureDirect         TargetSignatureKind = "direct"
	TargetSignatureCreate         TargetSignatureKind = "single-create"
	TargetSignatureBatch          TargetSignatureKind = "batch"
	TargetSignatureGraphPlan      TargetSignatureKind = "graph-plan"
	TargetSignatureDependencyEdge TargetSignatureKind = "dependency-edge"
	TargetSignatureChannel        TargetSignatureKind = "channel"
	TargetSignatureProcess        TargetSignatureKind = "process"
	TargetSignatureEventAppend    TargetSignatureKind = "event-append"
	TargetSignatureOperatorAttach TargetSignatureKind = "operator-terminal-attach"
	TargetSignatureTransaction    TargetSignatureKind = "transaction"
)

// TargetIdentityRole gives each identity component a semantic position in a
// target signature.
type TargetIdentityRole string

// TargetIdentityRole values enumerate the bounded signature components.
const (
	TargetRolePrimary          TargetIdentityRole = "primary"
	TargetRoleInput            TargetIdentityRole = "input"
	TargetRoleGenerated        TargetIdentityRole = "generated"
	TargetRolePlan             TargetIdentityRole = "plan"
	TargetRoleFrom             TargetIdentityRole = "from"
	TargetRoleTo               TargetIdentityRole = "to"
	TargetRoleOperatorTerminal TargetIdentityRole = "operator-terminal"
	TargetRoleDestination      TargetIdentityRole = "destination"
	TargetRoleCallback         TargetIdentityRole = "callback"
)

// TargetSourceKind identifies a mechanically inspectable target provenance.
type TargetSourceKind string

// TargetSourceKind values enumerate mechanically inspectable provenance.
const (
	TargetSourceBoundaryValue   TargetSourceKind = "boundary-value"
	TargetSourceObjectField     TargetSourceKind = "object-field"
	TargetSourceFunctionResult  TargetSourceKind = "function-result"
	TargetSourceStoreLiveReread TargetSourceKind = "store-live-reread"
	TargetSourceProcessScan     TargetSourceKind = "process-scan"
	TargetSourceChannelPayload  TargetSourceKind = "channel-payload"
	TargetSourceConfigValue     TargetSourceKind = "config-value"
	TargetSourceConstant        TargetSourceKind = "constant"
	TargetSourceAmbientTerminal TargetSourceKind = "ambient-terminal"
)

// ValueSlotKind identifies a receiver, parameter, result, or channel value.
type ValueSlotKind string

// ValueSlotKind values enumerate typed value positions.
const (
	SlotReceiver        ValueSlotKind = "receiver"
	SlotParameter       ValueSlotKind = "parameter"
	SlotResult          ValueSlotKind = "result"
	SlotChannelElement  ValueSlotKind = "channel-element"
	SlotBoundaryObject  ValueSlotKind = "boundary-object"
	SlotAmbientTerminal ValueSlotKind = "ambient-terminal"
)

// FenceKind names a current exclusion or stale-owner mechanism. Scope and
// cross-process guarantees are derived from this kind, never author asserted.
type FenceKind string

// FenceKind values enumerate current fence mechanisms and assumptions.
const (
	FenceNone                     FenceKind = "none"
	FenceProcessLocalNonExclusive FenceKind = "process-local-non-exclusive"
	FenceSingleWriterAssumption   FenceKind = "single-writer-assumption"
	FenceIdentifierFlock          FenceKind = "identifier-flock"
	FenceLiveRereadNonCAS         FenceKind = "live-reread-non-cas"
	FenceTokenRereadNonCAS        FenceKind = "token-reread-non-cas"
	FenceDurableGeneration        FenceKind = "durable-generation"
	FenceRevisionCAS              FenceKind = "revision-cas"
	FenceProviderAtomic           FenceKind = "provider-atomic"
	FenceCommandDedup             FenceKind = "command-dedup"
	FenceLeaseEpoch               FenceKind = "lease-epoch"
)

// FenceScope is the derived exclusion scope of a fence kind.
type FenceScope string

// FenceScope values describe derived mechanism scope without overclaiming.
const (
	FenceScopeNone         FenceScope = "none"
	FenceScopeProcess      FenceScope = "process"
	FenceScopeLockIdentity FenceScope = "lock-identity"
	FenceScopeDeployment   FenceScope = "deployment"
	FenceScopeStore        FenceScope = "store"
	FenceScopeTarget       FenceScope = "target"
	FenceScopeProvider     FenceScope = "provider"
)

// GateKind identifies the current effect-admission gate.
type GateKind string

// GateKind values enumerate current admission-gate forms.
const (
	GateUnconditionalLegacy GateKind = "unconditional-legacy"
	GatePredicate           GateKind = "typed-predicate"
	GateAll                 GateKind = "all"
	GateAny                 GateKind = "any"
)

// GateConditionKind identifies one typed input to a compound admission gate.
type GateConditionKind string

// GateConditionKind values distinguish direct predicates, exact authored
// parameters, and optional-interface capability checks on those parameters.
const (
	GateConditionPredicate  GateConditionKind = "predicate"
	GateConditionParameter  GateConditionKind = "parameter"
	GateConditionCapability GateConditionKind = "parameter-capability"
)

// Capability condition expectations are deliberately closed rather than
// accepting prose that cannot be compared mechanically.
const (
	GateCapabilityAvailable   = "available"
	GateCapabilityUnavailable = "unavailable"
)

// DispositionKind says whether a route is replaced, removed, or retained.
type DispositionKind string

// DispositionKind values enumerate future route treatment.
const (
	DispositionReplaceAtGate  DispositionKind = "replace-at-gate"
	DispositionRemoveAtGate   DispositionKind = "remove-at-gate"
	DispositionRetainBoundary DispositionKind = "retain-boundary"
)

// ContinuationLocus records where admitted work continues.
type ContinuationLocus string

// ContinuationLocus values enumerate effect continuation locations.
const (
	ContinuationInline        ContinuationLocus = "inline"
	ContinuationGoroutine     ContinuationLocus = "goroutine"
	ContinuationChannel       ContinuationLocus = "channel"
	ContinuationProviderChild ContinuationLocus = "provider-child"
)

// CompletionKind records how a caller learns that continued work is done.
type CompletionKind string

// CompletionKind values enumerate caller completion contracts.
const (
	CompletionSynchronous  CompletionKind = "synchronous"
	CompletionJoined       CompletionKind = "joined"
	CompletionRequestReply CompletionKind = "request-reply"
	CompletionDetached     CompletionKind = "detached"
)

// HopDispatchKind records whether a route edge is static or VTA-resolved.
type HopDispatchKind string

// HopDispatchKind values enumerate route-edge resolution modes.
const (
	HopDispatchExact HopDispatchKind = "exact"
	HopDispatchVTA   HopDispatchKind = "vta"
)

// ExceptionKind names a bounded, specifically testable legacy violation.
type ExceptionKind string

// ExceptionKind values enumerate bounded legacy violations.
const (
	ExceptionWeakFence            ExceptionKind = "weak-fence"
	ExceptionLegacyBypass         ExceptionKind = "legacy-bypass"
	ExceptionDestructiveCollision ExceptionKind = "destructive-collision"
	ExceptionBestEffortEvent      ExceptionKind = "best-effort-event"
	ExceptionDetachedContinuation ExceptionKind = "detached-continuation"
)

// AnchorKind identifies immutable exception-version evidence.
type AnchorKind string

// AnchorKind values enumerate immutable exception evidence.
const (
	AnchorGitCommit AnchorKind = "git-commit"
	AnchorGitTree   AnchorKind = "git-tree"
	AnchorSHA256    AnchorKind = "sha256"
)

// ObjectRef names an exact go/types object.
type ObjectRef struct {
	Package  string
	Receiver string
	Name     string
}

// FunctionRef identifies a named function or one of its lexical closures.
type FunctionRef struct {
	Object      ObjectRef
	File        string
	ClosurePath []int
}

// OperationSite locates an operation without unstable line numbers. Ordinal is
// one-based among operations of the same kind in this exact FunctionRef that
// resolve to the site's boundary or route-hop callee; nested closures do not
// contribute to their parent's ordinal.
type OperationSite struct {
	Operation OperationKind
	Enclosing FunctionRef
	Ordinal   int
}

// BoundaryDefinition is a closed discovery seed for the analyzer. For channel
// matching, zero Input and Output mean Object itself must have channel type.
// A channel-producing function parameter or result names its explicit
// one-based slot; Input and Output are mutually exclusive.
type BoundaryDefinition struct {
	ID     string
	Kind   EffectKind
	Object ObjectRef
	Match  ObjectMatchKind
	Input  ValueSlot
	Output ValueSlot
}

// SiteRegistration is one exact physical boundary crossing. Its identity is
// derived from BoundaryID and Matcher during compilation; authors never name
// physical sites. Cases classify the profiles in which discovery found it.
type SiteRegistration struct {
	BoundaryID string
	Matcher    OperationSite
	Cases      []ProfileCase
}

// ProfileCase is the one exhaustive routing case that applies to a physical
// site in each listed build profile. A case may have several distinct logical
// origins, but no profile may appear in more than one case for the site.
type ProfileCase struct {
	BuildProfiles []BuildProfileID
	Routes        []Route
}

// ValueSlot is one-based for parameters/results and index-free otherwise.
type ValueSlot struct {
	Kind  ValueSlotKind
	Index int
}

// TargetIdentityRef records one semantically named identity component, its
// authored boundary slot, optional field projection, and route provenance.
// Registry validation checks the closed shape and known-boundary policy; the
// analyzer must separately prove that the slot and projection resolve against
// the boundary's go/types signature before this evidence is production-gating.
type TargetIdentityRef struct {
	Role         TargetIdentityRole
	BoundarySlot ValueSlot
	Projection   ObjectRef
	Source       TargetSourceKind
	SourceObject ObjectRef
	SourceSlot   ValueSlot
}

// TargetRef records the affected identity, cardinality, bounded call
// signature, and typed provenance of every identity component.
type TargetRef struct {
	Kind        TargetKind
	Cardinality TargetCardinality
	Identity    TargetIdentityKind
	Signature   TargetSignatureKind
	Identities  []TargetIdentityRef
	// Detail is an optional operator note. It is deliberately excluded from
	// RouteID so wording changes cannot churn a safety identity.
	Detail string
}

// Fence records exact source/token objects for one fence mechanism.
type Fence struct {
	Kind   FenceKind
	Source ObjectRef
	Token  ObjectRef
}

// GateParameterRef identifies an exact authored function parameter whose value
// participates in admission.
type GateParameterRef struct {
	Function FunctionRef
	Slot     ValueSlot
}

// GateCondition records one typed predicate, parameter value, or capability
// assertion. Capability names a receiverless interface/type asserted against
// Parameter. Registry validation checks this authored shape; analyzer evidence
// must bind the function, slot, and capability type before a production gate.
type GateCondition struct {
	Kind       GateConditionKind
	Predicate  ObjectRef
	Parameter  GateParameterRef
	Capability ObjectRef
	Expected   string
}

// GateRef records an unconditional legacy path, the compact single-predicate
// form, or a typed all/any condition set.
type GateRef struct {
	Kind       GateKind
	Predicate  ObjectRef
	Expected   string
	Conditions []GateCondition
}

// TaskRef names a plan gate or beads task.
type TaskRef string

// Disposition records the future treatment of the current ownership route.
type Disposition struct {
	Kind   DispositionKind
	Gates  []TaskRef
	Reason string
}

// Continuation separates execution locus from completion/lifetime semantics.
type Continuation struct {
	Locus      ContinuationLocus
	Completion CompletionKind
}

// RouteHop identifies one authored origin-to-leaf call edge. Registry
// validation proves chain consistency; it does not rediscover callgraph edges.
type RouteHop struct {
	Site     OperationSite
	Dispatch HopDispatchKind
	Callee   FunctionRef
}

// TestRef names one exact top-level Go test.
type TestRef struct {
	Package string
	Name    string
}

// VersionAnchor pins an exception to immutable source or release evidence.
type VersionAnchor struct {
	Kind  AnchorKind
	Value string
}

// TemporaryException owns one bounded violation and its seam-specific proof.
type TemporaryException struct {
	Kind         ExceptionKind
	Reason       string
	OwnerTask    TaskRef
	RemovalTasks []TaskRef
	Anchor       VersionAnchor
	Expires      string
	OwningTest   TestRef
}

// Route classifies one logical origin and its authored call path to a
// registered physical site.
type Route struct {
	StoreDomain      StoreDomain
	ActionFamily     ActionFamily
	ExecutingProcess ExecutingProcess
	LogicalOwner     FunctionRef
	Target           TargetRef
	Fences           []Fence
	CurrentGate      GateRef
	Disposition      Disposition
	AccessPath       AccessPath
	Continuation     Continuation
	Hops             []RouteHop
	OwningTests      []TestRef
	Exception        *TemporaryException
}

// Registry is the single typed source of discovery boundaries, sites, and
// ownership routes.
type Registry struct {
	Boundaries    []BoundaryDefinition
	Registrations []SiteRegistration
}

// SiteRegistrationID is the content-derived identity of one physical site.
type SiteRegistrationID string

// RouteID is the content-derived identity of one site/profile route
// classification, including its complete current safety evidence.
type RouteID string

// CompiledRegistry is a validated, discovery-complete, canonically ordered
// registry. It is produced only when catalog and analyzer results agree in
// both directions.
type CompiledRegistry struct {
	Boundaries    []BoundaryDefinition
	Registrations []CompiledSiteRegistration
}

// CompiledSiteRegistration is one physical registration with a derived ID.
type CompiledSiteRegistration struct {
	ID         SiteRegistrationID
	BoundaryID string
	Matcher    OperationSite
	Cases      []CompiledProfileCase
}

// CompiledProfileCase is one canonically ordered profile classification.
type CompiledProfileCase struct {
	BuildProfiles []BuildProfileID
	Routes        []CompiledRoute
}

// CompiledRoute is one logical route classification with a derived ID.
type CompiledRoute struct {
	ID         RouteID
	Definition Route
}

// ProfileDiscovery records one completed analyzer run, including the valid
// zero-site result.
type ProfileDiscovery struct {
	Profile BuildProfileID
	Sites   []ObservedSite
}

// DiscoveryResult is the analyzer evidence reconciled by CompileRegistry.
// BoundaryDigest binds observations to the exact discovery vocabulary used to
// produce them; Profiles must contain every canonical analysis profile once.
// Production gates must construct this from the analyzer rather than from
// catalog-derived observations.
type DiscoveryResult struct {
	BoundaryDigest string
	Profiles       []ProfileDiscovery
}

type fencePolicy struct {
	Scope                  FenceScope
	SerializesSameIdentity bool
	RejectsStaleTarget     bool
	RejectsStaleOwner      bool
	SuppressesDuplicate    bool
	RequiresSource         bool
	RequiresToken          bool
}

type validatedRegistration struct {
	id           SiteRegistrationID
	registration SiteRegistration
	profiles     map[BuildProfileID]int
}

// CompileRegistry rejects incomplete, ambiguous, duplicate, stale, unknown, or
// expired classifications. It compiles only when discovery and registration
// cover the same exact physical sites in every build profile. Route call paths
// and target slot/projection claims receive structural validation here;
// callgraph evidence and go/types binding of those claims are outside this
// physical-site compiler and must precede a production catalog gate.
func CompileRegistry(registry Registry, discovery DiscoveryResult, asOf time.Time) (CompiledRegistry, error) {
	var problems []string
	validDate := !asOf.IsZero()
	if !validDate {
		problems = append(problems, "validation date is required")
	}
	if len(registry.Boundaries) == 0 {
		problems = append(problems, "registry has no boundaries")
	}
	if len(registry.Registrations) == 0 {
		problems = append(problems, "registry has no site registrations")
	}

	boundaries := validateBoundaries(registry.Boundaries, &problems)
	registrations := validateRegistrations(registry.Registrations, boundaries, asOf, validDate, &problems)
	reconcileDiscovery(registrations, discovery, deriveBoundaryDigest(registry.Boundaries), boundaries, &problems)

	if len(problems) == 0 {
		return compileValidatedRegistry(registry.Boundaries, registrations), nil
	}
	sort.Strings(problems)
	problems = compactStrings(problems)
	return CompiledRegistry{}, validationError(problems)
}

// ValidateRegistry validates and reconciles a registry against exact analyzer
// observations without retaining the compiled representation.
func ValidateRegistry(registry Registry, discovery DiscoveryResult, asOf time.Time) error {
	_, err := CompileRegistry(registry, discovery, asOf)
	return err
}

func validateBoundaries(definitions []BoundaryDefinition, problems *[]string) map[string]BoundaryDefinition {
	definitions = append([]BoundaryDefinition(nil), definitions...)
	sort.Slice(definitions, func(i, j int) bool {
		return canonicalBoundary(definitions[i]) < canonicalBoundary(definitions[j])
	})
	byID := make(map[string]BoundaryDefinition, len(definitions))
	objects := make(map[string]string, len(definitions))
	for _, boundary := range definitions {
		scope := fmt.Sprintf("boundary %q", deriveContentID("boundary-v1-", canonicalBoundary(boundary)))
		if strings.TrimSpace(boundary.ID) == "" {
			addProblem(problems, scope, "boundary id is required")
		}
		if !knownEffectKind(boundary.Kind) {
			addProblem(problems, scope, "unknown effect kind %q", boundary.Kind)
		}
		validateObject(boundary.Object, scope+" object", problems)
		if !oneOf(boundary.Match, ObjectMatchExact, ObjectMatchInterfaceImplementors, ObjectMatchChannel) {
			addProblem(problems, scope, "unknown object match %q", boundary.Match)
		}
		if boundary.Match == ObjectMatchInterfaceImplementors && boundary.Object.Receiver == "" {
			addProblem(problems, scope, "interface boundary requires a receiver")
		}
		if boundary.Match == ObjectMatchChannel && boundary.Kind != KindWakeSource {
			addProblem(problems, scope, "channel boundary must be a wake source")
		}
		if boundary.Match == ObjectMatchChannel {
			if !boundary.Input.zero() && !boundary.Output.zero() {
				addProblem(problems, scope, "channel boundary cannot name both input and output slots")
			}
			if !boundary.Input.zero() {
				validateExactSlot(boundary.Input, SlotParameter, scope+" channel input", problems)
			}
			if !boundary.Output.zero() {
				validateExactSlot(boundary.Output, SlotResult, scope+" channel output", problems)
			}
		} else {
			if !boundary.Input.zero() {
				addProblem(problems, scope, "non-channel boundary cannot name an input slot")
			}
			if !boundary.Output.zero() {
				addProblem(problems, scope, "non-channel boundary cannot name an output slot")
			}
		}
		if previous, exists := byID[boundary.ID]; exists {
			addProblem(problems, scope, "duplicate boundary id %q (first object %s)", boundary.ID, previous.Object.key())
		} else if boundary.ID != "" {
			byID[boundary.ID] = boundary
		}
		objectKey := canonicalObjectRef(boundary.Object)
		if previous, exists := objects[objectKey]; exists {
			addProblem(problems, scope, "boundary %q duplicates boundary object owned by %q", boundary.ID, previous)
		} else {
			objects[objectKey] = boundary.ID
		}
	}
	return byID
}

func validateRegistrations(registrations []SiteRegistration, boundaries map[string]BoundaryDefinition, asOf time.Time, validDate bool, problems *[]string) []validatedRegistration {
	result := make([]validatedRegistration, 0, len(registrations))
	physicalSites := make(map[string]SiteRegistrationID, len(registrations))
	totalRoutes := 0
	for _, registration := range registrations {
		registrationID := deriveSiteRegistrationID(registration.BoundaryID, registration.Matcher)
		scope := fmt.Sprintf("registration %q (%s)", registrationID, describePhysicalSite(registration.BoundaryID, registration.Matcher))
		boundary, boundaryExists := boundaries[registration.BoundaryID]
		if !boundaryExists {
			addProblem(problems, scope, "references unknown boundary %q", registration.BoundaryID)
		}
		validateOperationSite(registration.Matcher, scope+" matcher", problems)
		if boundaryExists {
			validateSiteBoundaryCompatibility(registration.Matcher, boundary, scope, problems)
		}

		physicalKey := registrationPhysicalKey(registration.BoundaryID, registration.Matcher)
		if previous, exists := physicalSites[physicalKey]; exists {
			addProblem(problems, scope, "duplicates physical site registration %q", previous)
		} else {
			physicalSites[physicalKey] = registrationID
		}

		profileCounts := make(map[BuildProfileID]int)
		if len(registration.Cases) == 0 {
			addProblem(problems, scope, "at least one profile classification case is required")
		}
		for _, profileCase := range registration.Cases {
			caseScope := fmt.Sprintf("%s case[%s]", scope, canonicalProfileKey(profileCase.BuildProfiles))
			validateProfileSet(profileCase.BuildProfiles, caseScope, "case", problems)
			for _, profile := range profileCase.BuildProfiles {
				profileCounts[profile]++
				if profileCounts[profile] == 2 {
					addProblem(problems, scope, "build profile %q has multiple classification cases", profile)
				}
			}
			if len(profileCase.Routes) == 0 {
				addProblem(problems, caseScope, "at least one logical route is required")
			}
			totalRoutes += len(profileCase.Routes)
			originCounts := make(map[string]int, len(profileCase.Routes))
			for _, route := range profileCase.Routes {
				routeID := deriveRouteID(registrationID, profileCase.BuildProfiles, route)
				routeScope := fmt.Sprintf("%s route %q", caseScope, routeID)
				validateRoute(route, registration.Matcher, boundary, boundaryExists, routeScope, asOf, validDate, problems)
				originKey := route.originKey()
				originCounts[originKey]++
				if originCounts[originKey] == 2 {
					originID := deriveContentID("origin-v1-", originKey)
					addProblem(problems, caseScope, "logical origin %q has multiple classifications", originID)
				}
			}
		}
		result = append(result, validatedRegistration{
			id:           registrationID,
			registration: registration,
			profiles:     profileCounts,
		})
	}
	if totalRoutes == 0 {
		*problems = append(*problems, "registry has no routes")
	}
	return result
}

func validateSiteBoundaryCompatibility(matcher OperationSite, boundary BoundaryDefinition, scope string, problems *[]string) {
	isChannelOperation := oneOf(matcher.Operation, OperationChannelSend, OperationChannelReceive, OperationSelectSend, OperationSelectReceive)
	if boundary.Match == ObjectMatchChannel && !isChannelOperation {
		addProblem(problems, scope, "channel boundary requires a channel operation")
	}
	if boundary.Match != ObjectMatchChannel && isChannelOperation {
		addProblem(problems, scope, "call boundary cannot use a channel operation")
	}
}

func validateRoute(route Route, matcher OperationSite, boundary BoundaryDefinition, boundaryExists bool, scope string, asOf time.Time, validDate bool, problems *[]string) {
	boundaryKind := EffectKind("")
	if boundaryExists {
		boundaryKind = boundary.Kind
		validateStoreDomain(route.StoreDomain, boundaryKind, scope, problems)
	}
	if !knownActionFamily(route.ActionFamily) {
		addProblem(problems, scope, "unknown action family %q", route.ActionFamily)
	}
	if !oneOf(route.ExecutingProcess, ProcessController, ProcessAPIInController, ProcessForegroundCLI, ProcessSidecarPoller, ProcessProviderChild) {
		addProblem(problems, scope, "unknown executing process %q", route.ExecutingProcess)
	}
	validateFunction(route.LogicalOwner, scope+" logical owner", problems)
	validateTarget(route.Target, boundaryKind, scope, problems)
	if boundaryExists {
		validateKnownBoundaryTarget(boundary, route.Target, scope, problems)
	}
	validateTargetActionFamily(route.Target, route.ActionFamily, scope, problems)
	validateFences(route.Fences, scope, problems)
	validateGate(route.CurrentGate, scope, problems)
	validateDisposition(route.Disposition, scope, problems)
	if !knownAccessPath(route.AccessPath) {
		addProblem(problems, scope, "unknown access path %q", route.AccessPath)
	}
	validateContinuation(route.Continuation, scope, problems)
	validateRouteHops(route, matcher, scope, problems)
	validateTests(route.OwningTests, scope, problems)
	if boundaryExists {
		validateAccessKind(route.AccessPath, boundaryKind, scope, problems)
	}
	validateException(route, boundaryKind, scope, asOf, validDate, problems)
}

func validateStoreDomain(domain StoreDomain, boundaryKind EffectKind, scope string, problems *[]string) {
	if boundaryKind == KindStoreMutation {
		if !knownStoreDomain(domain) {
			if domain == "" {
				addProblem(problems, scope, "store domain is required")
			} else {
				addProblem(problems, scope, "unknown store domain %q", domain)
			}
		}
		return
	}
	if domain != "" {
		addProblem(problems, scope, "store domain is only valid for store mutations")
	}
}

func reconcileDiscovery(registrations []validatedRegistration, discovery DiscoveryResult, expectedBoundaryDigest string, boundaries map[string]BoundaryDefinition, problems *[]string) {
	if discovery.BoundaryDigest != expectedBoundaryDigest {
		*problems = append(*problems, fmt.Sprintf("discovery boundary digest %q does not match registry digest %q", discovery.BoundaryDigest, expectedBoundaryDigest))
	}
	observed := validateDiscoveryProfiles(discovery.Profiles, problems)
	registrationsByPhysical := make(map[string][]validatedRegistration, len(registrations))
	classificationCounts := make(map[string]int)
	for _, registration := range registrations {
		physicalKey := registrationPhysicalKey(registration.registration.BoundaryID, registration.registration.Matcher)
		registrationsByPhysical[physicalKey] = append(registrationsByPhysical[physicalKey], registration)
		for profile, count := range registration.profiles {
			classificationCounts[siteProfileKey(physicalKey, profile)] += count
		}
	}

	observedCounts := make(map[string]int, len(observed))
	for _, site := range observed {
		registrationID := deriveSiteRegistrationID(site.BoundaryID, site.Matcher)
		scope := fmt.Sprintf("discovered site %q (%s) in build profile %q", registrationID, describePhysicalSite(site.BoundaryID, site.Matcher), site.Profile)
		boundary, boundaryExists := boundaries[site.BoundaryID]
		if !boundaryExists {
			addProblem(problems, scope, "references unknown boundary %q", site.BoundaryID)
		}
		validateOperationSite(site.Matcher, scope+" matcher", problems)
		if !knownBuildProfile(site.Profile) {
			addProblem(problems, scope, "unknown build profile %q", site.Profile)
		}
		if boundaryExists {
			validateSiteBoundaryCompatibility(site.Matcher, boundary, scope, problems)
		}

		physicalKey := registrationPhysicalKey(site.BoundaryID, site.Matcher)
		key := siteProfileKey(physicalKey, site.Profile)
		observedCounts[key]++
		if observedCounts[key] == 2 {
			addProblem(problems, scope, "was reported more than once by discovery")
		}
		matching := registrationsByPhysical[physicalKey]
		if len(matching) == 0 {
			addProblem(problems, scope, "has no registration")
			continue
		}
		if classificationCounts[key] == 0 {
			addProblem(problems, scope, "has no classification case")
		}
	}

	for _, registration := range registrations {
		physicalKey := registrationPhysicalKey(registration.registration.BoundaryID, registration.registration.Matcher)
		profiles := make([]BuildProfileID, 0, len(registration.profiles))
		for profile := range registration.profiles {
			profiles = append(profiles, profile)
		}
		sort.Slice(profiles, func(i, j int) bool { return profiles[i] < profiles[j] })
		for _, profile := range profiles {
			if observedCounts[siteProfileKey(physicalKey, profile)] == 0 {
				*problems = append(*problems, fmt.Sprintf(
					"stale registration %q (%s) in build profile %q: site was not discovered",
					registration.id,
					describePhysicalSite(registration.registration.BoundaryID, registration.registration.Matcher),
					profile,
				))
			}
		}
	}
}

func validateDiscoveryProfiles(discovery []ProfileDiscovery, problems *[]string) []ObservedSite {
	expected := make(map[BuildProfileID]bool)
	for _, profile := range canonicalAnalysisProfiles() {
		expected[profile.ID] = true
	}
	counts := make(map[BuildProfileID]int, len(discovery))
	var observed []ObservedSite
	for _, result := range discovery {
		scope := fmt.Sprintf("discovery profile %q", result.Profile)
		counts[result.Profile]++
		if !expected[result.Profile] {
			addProblem(problems, scope, "is not a canonical analysis profile")
		}
		if counts[result.Profile] == 2 {
			addProblem(problems, scope, "duplicate discovery profile")
		}
		for _, site := range result.Sites {
			if site.Profile != result.Profile {
				addProblem(problems, scope, "contains site labeled with build profile %q", site.Profile)
			}
			site.Profile = result.Profile
			observed = append(observed, site)
		}
	}
	for _, profile := range canonicalAnalysisProfiles() {
		if counts[profile.ID] == 0 {
			*problems = append(*problems, fmt.Sprintf("missing discovery profile %q", profile.ID))
		}
	}
	return observed
}

func compileValidatedRegistry(boundaries []BoundaryDefinition, registrations []validatedRegistration) CompiledRegistry {
	compiled := CompiledRegistry{
		Boundaries:    append([]BoundaryDefinition(nil), boundaries...),
		Registrations: make([]CompiledSiteRegistration, 0, len(registrations)),
	}
	sort.Slice(compiled.Boundaries, func(i, j int) bool {
		return compiled.Boundaries[i].ID < compiled.Boundaries[j].ID
	})
	for _, validated := range registrations {
		registration := validated.registration
		compiledRegistration := CompiledSiteRegistration{
			ID:         validated.id,
			BoundaryID: registration.BoundaryID,
			Matcher:    cloneOperationSite(registration.Matcher),
			Cases:      make([]CompiledProfileCase, 0, len(registration.Cases)),
		}
		for _, profileCase := range registration.Cases {
			compiledCase := CompiledProfileCase{
				BuildProfiles: append([]BuildProfileID(nil), profileCase.BuildProfiles...),
				Routes:        make([]CompiledRoute, 0, len(profileCase.Routes)),
			}
			for _, route := range profileCase.Routes {
				compiledCase.Routes = append(compiledCase.Routes, CompiledRoute{
					ID:         deriveRouteID(validated.id, profileCase.BuildProfiles, route),
					Definition: cloneRoute(route),
				})
			}
			sort.Slice(compiledCase.Routes, func(i, j int) bool {
				return compiledCase.Routes[i].ID < compiledCase.Routes[j].ID
			})
			compiledRegistration.Cases = append(compiledRegistration.Cases, compiledCase)
		}
		sort.Slice(compiledRegistration.Cases, func(i, j int) bool {
			return canonicalProfileKey(compiledRegistration.Cases[i].BuildProfiles) < canonicalProfileKey(compiledRegistration.Cases[j].BuildProfiles)
		})
		compiled.Registrations = append(compiled.Registrations, compiledRegistration)
	}
	sort.Slice(compiled.Registrations, func(i, j int) bool {
		return compiled.Registrations[i].ID < compiled.Registrations[j].ID
	})
	return compiled
}

func cloneRoute(route Route) Route {
	clone := route
	clone.LogicalOwner.ClosurePath = append([]int(nil), route.LogicalOwner.ClosurePath...)
	clone.Target = cloneTarget(route.Target)
	clone.CurrentGate = cloneGate(route.CurrentGate)
	clone.Fences = append([]Fence(nil), route.Fences...)
	clone.Disposition.Gates = append([]TaskRef(nil), route.Disposition.Gates...)
	clone.Hops = append([]RouteHop(nil), route.Hops...)
	for index := range clone.Hops {
		clone.Hops[index].Site.Enclosing.ClosurePath = append([]int(nil), route.Hops[index].Site.Enclosing.ClosurePath...)
		clone.Hops[index].Callee.ClosurePath = append([]int(nil), route.Hops[index].Callee.ClosurePath...)
	}
	clone.OwningTests = append([]TestRef(nil), route.OwningTests...)
	if route.Exception != nil {
		exception := *route.Exception
		exception.RemovalTasks = append([]TaskRef(nil), route.Exception.RemovalTasks...)
		clone.Exception = &exception
	}
	return clone
}

func cloneOperationSite(site OperationSite) OperationSite {
	clone := site
	clone.Enclosing.ClosurePath = append([]int(nil), site.Enclosing.ClosurePath...)
	return clone
}

func deriveSiteRegistrationID(boundaryID string, matcher OperationSite) SiteRegistrationID {
	return SiteRegistrationID(deriveContentID("site-v1-", registrationPhysicalKey(boundaryID, matcher)))
}

func deriveRouteID(registrationID SiteRegistrationID, profiles []BuildProfileID, route Route) RouteID {
	content := canonicalFields(
		"compiled-route-v2",
		string(registrationID),
		canonicalBuildProfiles(profiles),
		canonicalRoute(route),
	)
	return RouteID(deriveContentID("route-v2-", content))
}

func deriveContentID(prefix, canonical string) string {
	digest := sha256.Sum256([]byte(canonical))
	return prefix + fmt.Sprintf("%x", digest)
}

func registrationPhysicalKey(boundaryID string, matcher OperationSite) string {
	return canonicalFields("physical-site-v1", boundaryID, canonicalOperationSite(matcher))
}

func describePhysicalSite(boundaryID string, matcher OperationSite) string {
	return fmt.Sprintf(
		"boundary=%q operation=%q function=%q file=%q closure=%v ordinal=%d",
		boundaryID,
		matcher.Operation,
		matcher.Enclosing.Object.key(),
		matcher.Enclosing.File,
		matcher.Enclosing.ClosurePath,
		matcher.Ordinal,
	)
}

func siteProfileKey(physicalKey string, profile BuildProfileID) string {
	return canonicalFields("site-profile-v1", physicalKey, string(profile))
}

func canonicalProfileKey(profiles []BuildProfileID) string {
	values := append([]BuildProfileID(nil), profiles...)
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	parts := make([]string, len(values))
	for index, profile := range values {
		parts[index] = string(profile)
	}
	if len(parts) == 0 {
		return "<none>"
	}
	return strings.Join(parts, ",")
}

func deriveBoundaryDigest(boundaries []BoundaryDefinition) string {
	records := make([]string, len(boundaries))
	for index, boundary := range boundaries {
		records[index] = canonicalBoundary(boundary)
	}
	sort.Strings(records)
	return deriveContentID("boundaries-v1-", canonicalStringList("boundary-set-v1", records))
}

func canonicalBoundary(boundary BoundaryDefinition) string {
	return canonicalFields(
		"boundary-v1",
		boundary.ID,
		string(boundary.Kind),
		canonicalObjectRef(boundary.Object),
		string(boundary.Match),
		canonicalValueSlot(boundary.Input),
		canonicalValueSlot(boundary.Output),
	)
}

func canonicalRoute(route Route) string {
	fences := make([]string, len(route.Fences))
	for index, fence := range route.Fences {
		fences[index] = canonicalFence(fence)
	}
	sort.Strings(fences)

	hops := make([]string, len(route.Hops))
	for index, hop := range route.Hops {
		hops[index] = canonicalRouteHop(hop)
	}

	tests := make([]string, len(route.OwningTests))
	for index, test := range route.OwningTests {
		tests[index] = canonicalTestRef(test)
	}
	sort.Strings(tests)

	return canonicalFields(
		"route-classification-v2",
		string(route.StoreDomain),
		string(route.ActionFamily),
		string(route.ExecutingProcess),
		canonicalFunctionRef(route.LogicalOwner),
		canonicalTargetRef(route.Target),
		canonicalStringList("fences-v1", fences),
		canonicalGateRef(route.CurrentGate),
		canonicalDisposition(route.Disposition),
		string(route.AccessPath),
		canonicalContinuation(route.Continuation),
		canonicalStringList("route-hops-v1", hops),
		canonicalStringList("owning-tests-v1", tests),
		canonicalTemporaryException(route.Exception),
	)
}

func canonicalFence(fence Fence) string {
	return canonicalFields(
		"fence-v1",
		string(fence.Kind),
		canonicalObjectRef(fence.Source),
		canonicalObjectRef(fence.Token),
	)
}

func canonicalDisposition(disposition Disposition) string {
	gates := make([]string, len(disposition.Gates))
	for index, gate := range disposition.Gates {
		gates[index] = string(gate)
	}
	sort.Strings(gates)
	return canonicalFields(
		"disposition-v1",
		string(disposition.Kind),
		canonicalStringList("disposition-gates-v1", gates),
		disposition.Reason,
	)
}

func canonicalContinuation(continuation Continuation) string {
	return canonicalFields(
		"continuation-v1",
		string(continuation.Locus),
		string(continuation.Completion),
	)
}

func canonicalRouteHop(hop RouteHop) string {
	return canonicalFields(
		"route-hop-v1",
		canonicalOperationSite(hop.Site),
		string(hop.Dispatch),
		canonicalFunctionRef(hop.Callee),
	)
}

func canonicalTemporaryException(exception *TemporaryException) string {
	if exception == nil {
		return canonicalFields("temporary-exception-v1", "absent")
	}
	removalTasks := make([]string, len(exception.RemovalTasks))
	for index, task := range exception.RemovalTasks {
		removalTasks[index] = string(task)
	}
	sort.Strings(removalTasks)
	return canonicalFields(
		"temporary-exception-v1",
		"present",
		string(exception.Kind),
		exception.Reason,
		string(exception.OwnerTask),
		canonicalStringList("removal-tasks-v1", removalTasks),
		canonicalVersionAnchor(exception.Anchor),
		exception.Expires,
		canonicalTestRef(exception.OwningTest),
	)
}

func canonicalVersionAnchor(anchor VersionAnchor) string {
	return canonicalFields("version-anchor-v1", string(anchor.Kind), anchor.Value)
}

func canonicalTestRef(test TestRef) string {
	return canonicalFields("test-ref-v1", test.Package, test.Name)
}

func canonicalBuildProfiles(profiles []BuildProfileID) string {
	values := make([]string, len(profiles))
	for index, profile := range profiles {
		values[index] = string(profile)
	}
	sort.Strings(values)
	return canonicalStringList("build-profiles-v1", values)
}

func canonicalOperationSite(site OperationSite) string {
	return canonicalFields(
		"operation-site-v1",
		string(site.Operation),
		canonicalFunctionRef(site.Enclosing),
		strconv.Itoa(site.Ordinal),
	)
}

func canonicalFunctionRef(function FunctionRef) string {
	closure := make([]string, len(function.ClosurePath))
	for index, item := range function.ClosurePath {
		closure[index] = strconv.Itoa(item)
	}
	return canonicalFields(
		"function-ref-v1",
		canonicalObjectRef(function.Object),
		function.File,
		canonicalStringList("closure-path-v1", closure),
	)
}

func canonicalObjectRef(object ObjectRef) string {
	return canonicalFields("object-ref-v1", object.Package, object.Receiver, object.Name)
}

func canonicalValueSlot(slot ValueSlot) string {
	return canonicalFields("value-slot-v1", string(slot.Kind), strconv.Itoa(slot.Index))
}

func canonicalStringList(kind string, values []string) string {
	fields := make([]string, 0, len(values)+2)
	fields = append(fields, kind, strconv.Itoa(len(values)))
	fields = append(fields, values...)
	return canonicalFields(fields...)
}

func canonicalFields(values ...string) string {
	var result strings.Builder
	result.WriteString(strconv.Itoa(len(values)))
	result.WriteByte(';')
	for _, value := range values {
		result.WriteString(strconv.Itoa(len(value)))
		result.WriteByte(':')
		result.WriteString(value)
	}
	return result.String()
}

func validateFences(fences []Fence, scope string, problems *[]string) {
	if len(fences) == 0 {
		addProblem(problems, scope, "at least one fence disposition is required")
		return
	}
	seen := make(map[FenceKind]bool, len(fences))
	hasNone := false
	for index, fence := range fences {
		fenceScope := fmt.Sprintf("%s fence[%d]", scope, index)
		policy, known := fencePolicyFor(fence.Kind)
		if !known {
			addProblem(problems, fenceScope, "unknown fence %q", fence.Kind)
			continue
		}
		if seen[fence.Kind] {
			addProblem(problems, scope, "duplicate fence %q", fence.Kind)
		}
		seen[fence.Kind] = true
		hasNone = hasNone || fence.Kind == FenceNone
		if policy.RequiresSource {
			if fence.Source.zero() {
				addProblem(problems, fenceScope, "fence source is required")
			} else {
				validateObject(fence.Source, fenceScope+" source", problems)
			}
		} else if !fence.Source.zero() {
			addProblem(problems, fenceScope, "fence %s cannot name a source", fence.Kind)
		}
		if policy.RequiresToken {
			if fence.Token.zero() {
				addProblem(problems, fenceScope, "fence token is required")
			} else {
				validateObject(fence.Token, fenceScope+" token", problems)
			}
		} else if !fence.Token.zero() {
			addProblem(problems, fenceScope, "fence %s does not accept a token", fence.Kind)
		}
	}
	if hasNone && len(fences) != 1 {
		addProblem(problems, scope, "fence none must be the only fence")
	}
	if !sort.SliceIsSorted(fences, func(i, j int) bool { return fences[i].Kind < fences[j].Kind }) {
		addProblem(problems, scope, "fences must be sorted")
	}
}

func fencePolicyFor(kind FenceKind) (fencePolicy, bool) {
	switch kind {
	case FenceNone:
		return fencePolicy{Scope: FenceScopeNone}, true
	case FenceProcessLocalNonExclusive:
		return fencePolicy{Scope: FenceScopeProcess, SerializesSameIdentity: true, RequiresSource: true}, true
	case FenceSingleWriterAssumption:
		return fencePolicy{Scope: FenceScopeDeployment, RequiresSource: true}, true
	case FenceIdentifierFlock:
		return fencePolicy{Scope: FenceScopeLockIdentity, SerializesSameIdentity: true, RequiresSource: true, RequiresToken: true}, true
	case FenceLiveRereadNonCAS:
		return fencePolicy{Scope: FenceScopeStore, RequiresSource: true}, true
	case FenceTokenRereadNonCAS:
		return fencePolicy{Scope: FenceScopeTarget, RequiresSource: true, RequiresToken: true}, true
	case FenceDurableGeneration:
		return fencePolicy{Scope: FenceScopeTarget, RequiresSource: true, RequiresToken: true}, true
	case FenceRevisionCAS:
		return fencePolicy{Scope: FenceScopeTarget, RejectsStaleTarget: true, RequiresSource: true, RequiresToken: true}, true
	case FenceProviderAtomic:
		return fencePolicy{Scope: FenceScopeTarget, RejectsStaleTarget: true, RequiresSource: true, RequiresToken: true}, true
	case FenceCommandDedup:
		return fencePolicy{Scope: FenceScopeProvider, SuppressesDuplicate: true, RequiresSource: true, RequiresToken: true}, true
	case FenceLeaseEpoch:
		return fencePolicy{Scope: FenceScopeStore, RejectsStaleOwner: true, RequiresSource: true, RequiresToken: true}, true
	default:
		return fencePolicy{}, false
	}
}

func validateDisposition(disposition Disposition, scope string, problems *[]string) {
	if strings.TrimSpace(disposition.Reason) == "" {
		addProblem(problems, scope, "disposition reason is required")
	}
	validateTaskSet(disposition.Gates, scope+" disposition", problems)
	switch disposition.Kind {
	case DispositionReplaceAtGate, DispositionRemoveAtGate:
		if len(disposition.Gates) == 0 {
			addProblem(problems, scope, "replacement gates are required for %q", disposition.Kind)
		}
	case DispositionRetainBoundary:
		if len(disposition.Gates) != 0 {
			addProblem(problems, scope, "retained boundary cannot name replacement gates")
		}
	default:
		addProblem(problems, scope, "unknown disposition %q", disposition.Kind)
	}
}

func validateContinuation(continuation Continuation, scope string, problems *[]string) {
	switch continuation.Locus {
	case ContinuationInline:
		if continuation.Completion != CompletionSynchronous {
			addProblem(problems, scope, "inline continuation must complete synchronously")
		}
	case ContinuationGoroutine:
		if !oneOf(continuation.Completion, CompletionJoined, CompletionDetached) {
			addProblem(problems, scope, "goroutine continuation must be joined or detached")
		}
	case ContinuationChannel:
		if !oneOf(continuation.Completion, CompletionRequestReply, CompletionDetached) {
			addProblem(problems, scope, "channel continuation must be request-reply or detached")
		}
	case ContinuationProviderChild:
		if !oneOf(continuation.Completion, CompletionJoined, CompletionDetached) {
			addProblem(problems, scope, "provider-child continuation must be joined or detached")
		}
	default:
		addProblem(problems, scope, "unknown continuation locus %q", continuation.Locus)
	}
}

func validateRouteHops(route Route, matcher OperationSite, scope string, problems *[]string) {
	for index, hop := range route.Hops {
		hopScope := fmt.Sprintf("%s hop[%d]", scope, index)
		validateOperationSite(hop.Site, hopScope+" site", problems)
		if !oneOf(hop.Site.Operation, OperationCall, OperationGo, OperationDefer) {
			addProblem(problems, hopScope, "route hop must be call, go, or defer")
		}
		if !oneOf(hop.Dispatch, HopDispatchExact, HopDispatchVTA) {
			addProblem(problems, hopScope, "unknown hop dispatch %q", hop.Dispatch)
		}
		validateFunction(hop.Callee, hopScope+" callee", problems)
	}
	if len(route.Hops) == 0 {
		if !route.LogicalOwner.equal(matcher.Enclosing) {
			addProblem(problems, scope, "logical owner is not present in the exact route chain")
		}
		return
	}
	for index := 1; index < len(route.Hops); index++ {
		if !route.Hops[index-1].Callee.equal(route.Hops[index].Site.Enclosing) {
			addProblem(problems, scope, "route hop %d callee does not equal hop %d enclosing function", index-1, index)
		}
	}
	if !route.Hops[len(route.Hops)-1].Callee.equal(matcher.Enclosing) {
		addProblem(problems, scope, "last route hop must reach the physical site enclosing function")
	}
	if !routeContainsFunction(route, route.LogicalOwner) {
		addProblem(problems, scope, "logical owner is not present in the exact route chain")
	}
}

func validateException(route Route, boundaryKind EffectKind, scope string, asOf time.Time, validDate bool, problems *[]string) {
	exception := route.Exception
	access := route.AccessPath
	if exception == nil {
		if accessRequiresException(access) {
			addProblem(problems, scope, "%s requires a temporary exception", access)
		}
		return
	}
	if accessRequiresException(access) {
		if route.Disposition.Kind == DispositionRetainBoundary {
			addProblem(problems, scope, "expiring bypass cannot retain its ownership route")
		}
		if !equalTaskSets(exception.RemovalTasks, route.Disposition.Gates) {
			addProblem(problems, scope, "exception removal tasks must equal disposition gates")
		}
	}
	exceptionScope := scope + " exception"
	if !oneOf(exception.Kind, ExceptionWeakFence, ExceptionLegacyBypass, ExceptionDestructiveCollision, ExceptionBestEffortEvent, ExceptionDetachedContinuation) {
		addProblem(problems, exceptionScope, "unknown exception kind %q", exception.Kind)
	}
	switch exception.Kind {
	case ExceptionWeakFence:
		if !hasWeakFence(route.Fences) {
			addProblem(problems, exceptionScope, "weak-fence exception requires a weak fence disposition")
		}
	case ExceptionLegacyBypass:
		if !accessRequiresException(access) {
			addProblem(problems, exceptionScope, "legacy-bypass exception requires a bypass access path")
		}
	case ExceptionDestructiveCollision:
		if !oneOf(boundaryKind, KindProviderMutation, KindProcessMutation) {
			addProblem(problems, exceptionScope, "destructive-collision exception requires a provider/process boundary")
		}
	case ExceptionBestEffortEvent:
		if boundaryKind != KindEventEmission {
			addProblem(problems, exceptionScope, "best-effort-event exception requires an event boundary")
		}
	case ExceptionDetachedContinuation:
		if route.Continuation.Completion != CompletionDetached {
			addProblem(problems, exceptionScope, "detached-continuation exception requires detached completion")
		}
	}
	if strings.TrimSpace(exception.Reason) == "" {
		addProblem(problems, exceptionScope, "exception reason is required")
	}
	validateTask(exception.OwnerTask, exceptionScope+" owner", problems)
	if len(exception.RemovalTasks) == 0 {
		addProblem(problems, exceptionScope, "exception removal tasks are required")
	} else {
		validateTaskSet(exception.RemovalTasks, exceptionScope+" removal", problems)
	}
	validateAnchor(exception.Anchor, exceptionScope, problems)
	validateTest(exception.OwningTest, exceptionScope+" owning test", problems)
	expires, err := time.Parse("2006-01-02", exception.Expires)
	if err != nil {
		addProblem(problems, exceptionScope, "exception expiry %q must use YYYY-MM-DD", exception.Expires)
		return
	}
	if !validDate {
		return
	}
	asOf = dayUTC(asOf)
	if expires.Before(asOf) {
		addProblem(problems, exceptionScope, "exception expired on %s", exception.Expires)
	}
}

func validateAnchor(anchor VersionAnchor, scope string, problems *[]string) {
	switch anchor.Kind {
	case AnchorGitCommit:
		if !lowerHex(anchor.Value, 40) {
			addProblem(problems, scope, "git commit anchor must be 40 lowercase hexadecimal characters")
		}
	case AnchorGitTree:
		if !lowerHex(anchor.Value, 40) {
			addProblem(problems, scope, "git tree anchor must be 40 lowercase hexadecimal characters")
		}
	case AnchorSHA256:
		if !lowerHex(anchor.Value, 64) {
			addProblem(problems, scope, "sha256 anchor must be 64 lowercase hexadecimal characters")
		}
	default:
		addProblem(problems, scope, "unknown version anchor %q", anchor.Kind)
	}
}

func validateAccessKind(access AccessPath, kind EffectKind, scope string, problems *[]string) {
	switch access {
	case AccessSessionStoreFrontDoor, AccessRawStoreBypass, AccessStoreAdapter:
		if kind != KindStoreMutation {
			addProblem(problems, scope, "%s requires a store-mutation boundary", access)
		}
	case AccessProcessBoundary, AccessDirectProcessBypass:
		if kind != KindProcessMutation {
			addProblem(problems, scope, "%s requires a process-mutation boundary", access)
		}
	case AccessDirectEvent:
		if kind != KindEventEmission {
			addProblem(problems, scope, "direct-event requires an event-emission boundary")
		}
	case AccessDirectWake:
		if kind != KindWakeSource {
			addProblem(problems, scope, "direct-wake requires a wake-source boundary")
		}
	case AccessProviderNative:
		if !oneOf(kind, KindProviderMutation, KindProcessMutation) {
			addProblem(problems, scope, "provider-native-internal requires a provider/process boundary")
		}
	case AccessProviderBypass:
		if kind != KindProviderMutation {
			addProblem(problems, scope, "provider-bypass requires a provider-mutation boundary")
		}
	case AccessWorkerBoundary:
		if !oneOf(kind, KindStoreMutation, KindProviderMutation) {
			addProblem(problems, scope, "worker-boundary requires a store/provider boundary")
		}
	case AccessManagerBypass:
		if !oneOf(kind, KindStoreMutation, KindProviderMutation, KindProcessMutation) {
			addProblem(problems, scope, "manager-bypass requires a store/provider/process boundary")
		}
	}
}

func validateProfileSet(profiles []BuildProfileID, scope, owner string, problems *[]string) {
	if len(profiles) == 0 {
		addProblem(problems, scope, "%s build profiles are required", owner)
		return
	}
	seen := make(map[BuildProfileID]bool, len(profiles))
	for _, profile := range profiles {
		if !knownBuildProfile(profile) {
			addProblem(problems, scope, "unknown build profile %q", profile)
		}
		if seen[profile] {
			addProblem(problems, scope, "duplicate build profile %q", profile)
		}
		seen[profile] = true
	}
	if !sort.SliceIsSorted(profiles, func(i, j int) bool { return profiles[i] < profiles[j] }) {
		addProblem(problems, scope, "build profiles must be sorted")
	}
}

func validateOperationSite(site OperationSite, scope string, problems *[]string) {
	if !oneOf(site.Operation, OperationCall, OperationGo, OperationDefer, OperationChannelSend, OperationChannelReceive, OperationSelectSend, OperationSelectReceive) {
		addProblem(problems, scope, "unknown operation %q", site.Operation)
	}
	validateFunction(site.Enclosing, scope+" enclosing", problems)
	if site.Ordinal <= 0 {
		addProblem(problems, scope, "operation ordinal must be positive")
	}
}

func validateFunction(function FunctionRef, scope string, problems *[]string) {
	validateObject(function.Object, scope+" object", problems)
	if !cleanRepoPath(function.File) {
		addProblem(problems, scope, "function file %q must be a clean repository-relative slash path", function.File)
	}
	for index, item := range function.ClosurePath {
		if item <= 0 {
			addProblem(problems, scope, "closure path item %d must be positive", index)
		}
	}
}

func validateObject(object ObjectRef, scope string, problems *[]string) {
	if strings.TrimSpace(object.Package) == "" {
		addProblem(problems, scope, "%s package is required", lastWords(scope, 3))
	}
	if strings.TrimSpace(object.Name) == "" {
		addProblem(problems, scope, "%s name is required", lastWords(scope, 3))
	} else if !token.IsIdentifier(object.Name) {
		addProblem(problems, scope, "%s name %q must be a Go identifier", lastWords(scope, 3), object.Name)
	}
	if object.Receiver != "" && !token.IsIdentifier(object.Receiver) {
		addProblem(problems, scope, "%s receiver %q must be a Go identifier", lastWords(scope, 3), object.Receiver)
	}
}

func validateExactSlot(slot ValueSlot, want ValueSlotKind, scope string, problems *[]string) {
	if slot.Kind != want {
		addProblem(problems, scope, "slot must be %q", want)
		return
	}
	validateSlotIndex(slot, scope, problems)
}

func validateSlotIndex(slot ValueSlot, scope string, problems *[]string) {
	switch slot.Kind {
	case SlotParameter, SlotResult:
		if slot.Index <= 0 {
			addProblem(problems, scope, "%s slot index must be positive", slot.Kind)
		}
	case SlotReceiver, SlotChannelElement, SlotBoundaryObject, SlotAmbientTerminal:
		if slot.Index != 0 {
			addProblem(problems, scope, "%s slot cannot have an index", slot.Kind)
		}
	}
}

func validateTests(tests []TestRef, scope string, problems *[]string) {
	if len(tests) == 0 {
		addProblem(problems, scope, "at least one owning test is required")
		return
	}
	seen := make(map[string]bool, len(tests))
	for index, test := range tests {
		validateTest(test, fmt.Sprintf("%s owning test[%d]", scope, index), problems)
		if seen[test.key()] {
			addProblem(problems, scope, "duplicate owning test %s", test.key())
		}
		seen[test.key()] = true
	}
	if !sort.SliceIsSorted(tests, func(i, j int) bool { return tests[i].key() < tests[j].key() }) {
		addProblem(problems, scope, "owning tests must be sorted")
	}
}

func validateTest(test TestRef, scope string, problems *[]string) {
	if strings.TrimSpace(test.Package) == "" {
		addProblem(problems, scope, "%s package is required", lastWords(scope, 2))
	}
	if !strings.HasPrefix(test.Name, "Test") || len(test.Name) == len("Test") {
		addProblem(problems, scope, "%s name must be a top-level Test function", lastWords(scope, 2))
	}
}

func validateTaskSet(tasks []TaskRef, scope string, problems *[]string) {
	seen := make(map[TaskRef]bool, len(tasks))
	for _, task := range tasks {
		validateTask(task, scope, problems)
		if seen[task] {
			addProblem(problems, scope, "duplicate task %q", task)
		}
		seen[task] = true
	}
	if !sort.SliceIsSorted(tasks, func(i, j int) bool { return tasks[i] < tasks[j] }) {
		addProblem(problems, scope, "tasks must be sorted")
	}
}

func validateTask(task TaskRef, scope string, problems *[]string) {
	value := string(task)
	if strings.HasPrefix(value, "ga-") && len(value) > 3 {
		return
	}
	if len(value) >= 2 && (value[0] == 'P' || value[0] == 'G') && value[1] >= '0' && value[1] <= '9' {
		for _, char := range value[2:] {
			if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
				(char >= '0' && char <= '9') || char == '.' || char == '-' {
				continue
			}
			addProblem(problems, scope, "invalid task reference %q", task)
			return
		}
		return
	}
	addProblem(problems, scope, "invalid task reference %q", task)
}

func knownEffectKind(value EffectKind) bool {
	return oneOf(value, KindStoreMutation, KindProviderMutation, KindProcessMutation, KindEventEmission, KindWakeSource)
}

func knownStoreDomain(value StoreDomain) bool {
	return oneOf(value, StoreDomainSessionLifecycle, StoreDomainWaitIntent, StoreDomainNudgeIntent, StoreDomainRouteRecovery, StoreDomainPoolRouting, StoreDomainControlDispatch, StoreDomainOrderDispatch, StoreDomainMaintenance)
}

func knownBuildProfile(value BuildProfileID) bool {
	_, ok := canonicalAnalysisProfile(value)
	return ok
}

func knownActionFamily(value ActionFamily) bool {
	return oneOf(value,
		FamilyStatusHeal, FamilyIdentityHealRetirement, FamilyPrimePromptDelivery,
		FamilyInterruptStopTurn, FamilyStartInitiation, FamilyStartConfirmationAdoption,
		FamilyDrainBeginCancel, FamilyDrainAckCompletion, FamilyTimersWake,
		FamilyLiveConfig, FamilyRestartGeneration, FamilyProviderSwap, FamilyStop,
		FamilyCloseWorkRelease, FamilyNudge, FamilyWaitIntent, FamilyPoolScale,
		FamilyRouteRecovery, FamilyControlDispatch, FamilyOrderDispatch,
		FamilyMaintenance, FamilyObservation, FamilyControllerWake,
		FamilyRuntimeProvision, FamilyRuntimeLaunch, FamilyRuntimeTeardown,
		FamilyServerTeardown, FamilyProcessSignal, FamilyOperatorAttach,
	)
}

func knownAccessPath(value AccessPath) bool {
	return oneOf(value, AccessWorkerBoundary, AccessSessionStoreFrontDoor, AccessManagerBypass, AccessProviderBypass, AccessRawStoreBypass, AccessStoreAdapter, AccessProviderNative, AccessProcessBoundary, AccessDirectProcessBypass, AccessDirectEvent, AccessDirectWake)
}

func accessRequiresException(access AccessPath) bool {
	return oneOf(access, AccessManagerBypass, AccessProviderBypass, AccessRawStoreBypass, AccessDirectProcessBypass)
}

func routeContainsFunction(route Route, target FunctionRef) bool {
	for _, hop := range route.Hops {
		if hop.Site.Enclosing.equal(target) || hop.Callee.equal(target) {
			return true
		}
	}
	return false
}

func equalTaskSets(left, right []TaskRef) bool {
	if len(left) != len(right) {
		return false
	}
	want := make(map[TaskRef]int, len(left))
	for _, task := range left {
		want[task]++
	}
	for _, task := range right {
		want[task]--
		if want[task] < 0 {
			return false
		}
	}
	return true
}

func hasWeakFence(fences []Fence) bool {
	for _, fence := range fences {
		if oneOf(fence.Kind,
			FenceNone,
			FenceProcessLocalNonExclusive,
			FenceSingleWriterAssumption,
			FenceIdentifierFlock,
			FenceLiveRereadNonCAS,
			FenceTokenRereadNonCAS,
			FenceDurableGeneration,
		) {
			return true
		}
	}
	return false
}

func lowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func cleanRepoPath(value string) bool {
	return value != "" && !path.IsAbs(value) && path.Clean(value) == value && value != "." && !strings.HasPrefix(value, "../")
}

func dayUTC(value time.Time) time.Time {
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

func oneOf[T comparable](value T, candidates ...T) bool {
	for _, candidate := range candidates {
		if value == candidate {
			return true
		}
	}
	return false
}

func contains[T comparable](values []T, want T) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (route Route) originKey() string {
	hops := make([]string, len(route.Hops))
	for index, hop := range route.Hops {
		hops[index] = canonicalRouteHop(hop)
	}
	return canonicalFields(
		"logical-origin-v1",
		canonicalFunctionRef(route.LogicalOwner),
		canonicalStringList("logical-origin-hops-v1", hops),
	)
}

func (site OperationSite) key() string {
	return strings.Join([]string{string(site.Operation), site.Enclosing.key(), fmt.Sprint(site.Ordinal)}, "|")
}

func (function FunctionRef) key() string {
	closure := make([]string, len(function.ClosurePath))
	for index, item := range function.ClosurePath {
		closure[index] = fmt.Sprint(item)
	}
	return strings.Join([]string{function.Object.key(), function.File, strings.Join(closure, ".")}, "|")
}

func (function FunctionRef) equal(other FunctionRef) bool {
	return canonicalFunctionRef(function) == canonicalFunctionRef(other)
}

func (object ObjectRef) key() string {
	return strings.Join([]string{object.Package, object.Receiver, object.Name}, ".")
}

func (object ObjectRef) zero() bool {
	return object == (ObjectRef{})
}

func (slot ValueSlot) zero() bool {
	return slot == (ValueSlot{})
}

func (test TestRef) key() string {
	return test.Package + "#" + test.Name
}

func addProblem(problems *[]string, scope, format string, args ...any) {
	*problems = append(*problems, scope+": "+fmt.Sprintf(format, args...))
}

func lastWords(value string, count int) string {
	parts := strings.Fields(value)
	if len(parts) <= count {
		return value
	}
	return strings.Join(parts[len(parts)-count:], " ")
}

type validationError []string

func (e validationError) Error() string {
	return "effect registry validation failed:\n- " + strings.Join(e, "\n- ")
}
