// Package effectinventory owns the mechanically checked registry of production
// reconciler effects. The registry describes current ownership; it does not
// execute or authorize effects.
package effectinventory

import (
	"fmt"
	"path"
	"sort"
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
	TargetDurableRecord     TargetKind = "durable-record"
	TargetSessionIdentity   TargetKind = "session-identity"
	TargetRuntimeIdentity   TargetKind = "runtime-identity"
	TargetProcessIdentity   TargetKind = "process-identity"
	TargetProviderServer    TargetKind = "provider-server"
	TargetEventLog          TargetKind = "event-log"
	TargetControllerChannel TargetKind = "controller-channel"
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
)

// ValueSlotKind identifies a receiver, parameter, result, or channel value.
type ValueSlotKind string

// ValueSlotKind values enumerate typed value positions.
const (
	SlotReceiver       ValueSlotKind = "receiver"
	SlotParameter      ValueSlotKind = "parameter"
	SlotResult         ValueSlotKind = "result"
	SlotChannelElement ValueSlotKind = "channel-element"
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
// matching, a zero Output means Object itself must have channel type; a
// function or method result always names its explicit one-based result slot.
type BoundaryDefinition struct {
	ID     string
	Kind   EffectKind
	Object ObjectRef
	Match  ObjectMatchKind
	Output ValueSlot
}

// Site is one physical boundary crossing, deduplicated across routes.
type Site struct {
	ID            string
	BoundaryID    string
	StoreDomain   StoreDomain
	Matcher       OperationSite
	BuildProfiles []BuildProfileID
}

// ValueSlot is one-based for parameters/results and index-free otherwise.
type ValueSlot struct {
	Kind  ValueSlotKind
	Index int
}

// TargetRef records both the affected identity and its typed provenance.
type TargetRef struct {
	Kind         TargetKind
	Sink         ValueSlot
	Source       TargetSourceKind
	SourceObject ObjectRef
	SourceSlot   ValueSlot
	Detail       string
}

// Fence records exact source/token objects for one fence mechanism.
type Fence struct {
	Kind   FenceKind
	Source ObjectRef
	Token  ObjectRef
}

// GateRef records an unconditional legacy path or an exact typed predicate.
type GateRef struct {
	Kind      GateKind
	Predicate ObjectRef
	Expected  string
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

// RouteHop identifies one exact origin-to-leaf call edge.
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

// Route classifies one logical origin and its exact call path to a Site.
type Route struct {
	ID               string
	SiteID           string
	BuildProfiles    []BuildProfileID
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
	Boundaries []BoundaryDefinition
	Sites      []Site
	Routes     []Route
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

type seenRoute struct {
	id       string
	profiles map[BuildProfileID]bool
}

// ValidateRegistry rejects incomplete, ambiguous, duplicate, or expired
// classifications and returns every problem in deterministic order.
func ValidateRegistry(registry Registry, asOf time.Time) error {
	var problems []string
	validDate := !asOf.IsZero()
	if !validDate {
		problems = append(problems, "validation date is required")
	}
	if len(registry.Boundaries) == 0 {
		problems = append(problems, "registry has no boundaries")
	}
	if len(registry.Sites) == 0 {
		problems = append(problems, "registry has no sites")
	}
	if len(registry.Routes) == 0 {
		problems = append(problems, "registry has no routes")
	}

	boundaries := validateBoundaries(registry.Boundaries, &problems)
	sites := validateSites(registry.Sites, boundaries, &problems)
	validateRoutes(registry.Routes, sites, boundaries, asOf, validDate, &problems)
	validateCoverage(registry.Sites, registry.Routes, &problems)

	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return validationError(problems)
}

func validateBoundaries(definitions []BoundaryDefinition, problems *[]string) map[string]BoundaryDefinition {
	byID := make(map[string]BoundaryDefinition, len(definitions))
	objects := make(map[string]string, len(definitions))
	for index, boundary := range definitions {
		scope := fmt.Sprintf("boundary[%d]", index)
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
			if !boundary.Output.zero() {
				validateExactSlot(boundary.Output, SlotResult, scope+" channel output", problems)
			}
		} else if !boundary.Output.zero() {
			addProblem(problems, scope, "non-channel boundary cannot name an output slot")
		}
		if previous, exists := byID[boundary.ID]; exists {
			addProblem(problems, scope, "duplicate boundary id %q (first object %s)", boundary.ID, previous.Object.key())
		} else if boundary.ID != "" {
			byID[boundary.ID] = boundary
		}
		objectKey := boundary.Object.key()
		if previous, exists := objects[objectKey]; exists {
			addProblem(problems, scope, "boundary %q duplicates boundary object owned by %q", boundary.ID, previous)
		} else {
			objects[objectKey] = boundary.ID
		}
	}
	return byID
}

func validateSites(siteList []Site, boundaries map[string]BoundaryDefinition, problems *[]string) map[string]Site {
	byID := make(map[string]Site, len(siteList))
	matchers := make(map[string]string, len(siteList))
	for index, site := range siteList {
		scope := fmt.Sprintf("site[%d]", index)
		if strings.TrimSpace(site.ID) == "" {
			addProblem(problems, scope, "site id is required")
		}
		boundary, boundaryExists := boundaries[site.BoundaryID]
		if !boundaryExists {
			addProblem(problems, scope, "site %q references unknown boundary %q", site.ID, site.BoundaryID)
		}
		validateOperationSite(site.Matcher, scope+" matcher", problems)
		validateProfileSet(site.BuildProfiles, scope, "site", problems)
		if boundaryExists {
			validateSiteBoundaryCompatibility(site, boundary, scope, problems)
		}
		if previous, exists := byID[site.ID]; exists {
			addProblem(problems, scope, "duplicate site id %q (first boundary %q)", site.ID, previous.BoundaryID)
		} else if site.ID != "" {
			byID[site.ID] = site
		}
		matcherKey := site.BoundaryID + "|" + site.Matcher.key()
		if previous, exists := matchers[matcherKey]; exists {
			addProblem(problems, scope, "duplicate physical site matcher in %q and %q", previous, site.ID)
		} else {
			matchers[matcherKey] = site.ID
		}
	}
	return byID
}

func validateSiteBoundaryCompatibility(site Site, boundary BoundaryDefinition, scope string, problems *[]string) {
	isChannelOperation := oneOf(site.Matcher.Operation, OperationChannelSend, OperationChannelReceive, OperationSelectSend, OperationSelectReceive)
	if boundary.Match == ObjectMatchChannel && !isChannelOperation {
		addProblem(problems, scope, "channel boundary requires a channel operation")
	}
	if boundary.Match != ObjectMatchChannel && isChannelOperation {
		addProblem(problems, scope, "call boundary cannot use a channel operation")
	}
	if boundary.Kind == KindStoreMutation {
		if !knownStoreDomain(site.StoreDomain) {
			if site.StoreDomain == "" {
				addProblem(problems, scope, "store domain is required")
			} else {
				addProblem(problems, scope, "unknown store domain %q", site.StoreDomain)
			}
		}
	} else if site.StoreDomain != "" {
		addProblem(problems, scope, "store domain is only valid for store mutations")
	}
}

func validateRoutes(routes []Route, sites map[string]Site, boundaries map[string]BoundaryDefinition, asOf time.Time, validDate bool, problems *[]string) {
	ids := make(map[string]int, len(routes))
	semanticRoutes := make(map[string]seenRoute, len(routes))
	for index, route := range routes {
		scope := fmt.Sprintf("route[%d]", index)
		if strings.TrimSpace(route.ID) == "" {
			addProblem(problems, scope, "route id is required")
		}
		if previous, exists := ids[route.ID]; exists {
			addProblem(problems, scope, "duplicate route id %q (first at route[%d])", route.ID, previous)
		} else if route.ID != "" {
			ids[route.ID] = index
		}
		site, siteExists := sites[route.SiteID]
		if !siteExists {
			addProblem(problems, scope, "route %q references unknown site %q", route.ID, route.SiteID)
		}
		validateProfileSet(route.BuildProfiles, scope, "route", problems)
		if siteExists {
			validateRouteProfiles(route, site, scope, problems)
		}
		if !knownActionFamily(route.ActionFamily) {
			addProblem(problems, scope, "unknown action family %q", route.ActionFamily)
		}
		if !oneOf(route.ExecutingProcess, ProcessController, ProcessAPIInController, ProcessForegroundCLI, ProcessSidecarPoller, ProcessProviderChild) {
			addProblem(problems, scope, "unknown executing process %q", route.ExecutingProcess)
		}
		validateFunction(route.LogicalOwner, scope+" logical owner", problems)
		validateTarget(route.Target, scope, problems)
		validateFences(route.Fences, scope, problems)
		validateGate(route.CurrentGate, scope, problems)
		validateDisposition(route.Disposition, scope, problems)
		if !knownAccessPath(route.AccessPath) {
			addProblem(problems, scope, "unknown access path %q", route.AccessPath)
		}
		validateContinuation(route.Continuation, scope, problems)
		validateRouteHops(route, site, siteExists, scope, problems)
		validateTests(route.OwningTests, scope, problems)
		boundaryKind := EffectKind("")
		if siteExists {
			boundaryKind = boundaries[site.BoundaryID].Kind
			validateAccessKind(route.AccessPath, boundaryKind, scope, problems)
		}
		validateException(route, boundaryKind, scope, asOf, validDate, problems)

		semanticKey := route.semanticKey()
		if previous, exists := semanticRoutes[semanticKey]; exists {
			overlap := overlappingProfiles(previous.profiles, route.BuildProfiles)
			if len(overlap) > 0 {
				addProblem(problems, scope, "route %q duplicates semantic route %q in build profiles %q", route.ID, previous.id, overlap)
			}
			for _, profile := range route.BuildProfiles {
				previous.profiles[profile] = true
			}
			semanticRoutes[semanticKey] = previous
		} else {
			semanticRoutes[semanticKey] = seenRoute{id: route.ID, profiles: profileSet(route.BuildProfiles)}
		}
	}
}

func validateRouteProfiles(route Route, site Site, scope string, problems *[]string) {
	siteProfiles := stringSet(site.BuildProfiles)
	for _, profile := range route.BuildProfiles {
		if !siteProfiles[string(profile)] {
			addProblem(problems, scope, "route build profile %q is not present on site %q", profile, site.ID)
		}
	}
}

func validateTarget(target TargetRef, scope string, problems *[]string) {
	if !oneOf(target.Kind, TargetDurableRecord, TargetSessionIdentity, TargetRuntimeIdentity, TargetProcessIdentity, TargetProviderServer, TargetEventLog, TargetControllerChannel) {
		addProblem(problems, scope, "unknown target kind %q", target.Kind)
	}
	validateSinkSlot(target.Sink, scope+" target sink", problems)
	if !oneOf(target.Source, TargetSourceBoundaryValue, TargetSourceObjectField, TargetSourceFunctionResult, TargetSourceStoreLiveReread, TargetSourceProcessScan, TargetSourceChannelPayload, TargetSourceConfigValue, TargetSourceConstant) {
		addProblem(problems, scope, "unknown target source %q", target.Source)
		return
	}
	requiresObject := target.Source != TargetSourceBoundaryValue
	if requiresObject {
		validateObject(target.SourceObject, scope+" target source object", problems)
	} else if !target.SourceObject.zero() {
		addProblem(problems, scope, "boundary-value target source cannot name an object")
	}
	switch target.Source {
	case TargetSourceBoundaryValue, TargetSourceObjectField, TargetSourceConfigValue, TargetSourceConstant:
		if !target.SourceSlot.zero() {
			addProblem(problems, scope, "target source %q cannot name a source slot", target.Source)
		}
	case TargetSourceFunctionResult, TargetSourceStoreLiveReread, TargetSourceProcessScan:
		validateExactSlot(target.SourceSlot, SlotResult, scope+" target source", problems)
	case TargetSourceChannelPayload:
		validateExactSlot(target.SourceSlot, SlotChannelElement, scope+" target source", problems)
	}
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

func validateGate(gate GateRef, scope string, problems *[]string) {
	switch gate.Kind {
	case GateUnconditionalLegacy:
		if !gate.Predicate.zero() {
			addProblem(problems, scope, "unconditional gate cannot name a predicate")
		}
		if gate.Expected != "" {
			addProblem(problems, scope, "unconditional gate cannot name an expected value")
		}
	case GatePredicate:
		validateObject(gate.Predicate, scope+" gate predicate", problems)
		if strings.TrimSpace(gate.Expected) == "" {
			addProblem(problems, scope, "gate expected value is required")
		}
	default:
		addProblem(problems, scope, "unknown current gate %q", gate.Kind)
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

func validateRouteHops(route Route, site Site, siteExists bool, scope string, problems *[]string) {
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
	if !siteExists {
		return
	}
	if len(route.Hops) == 0 {
		if !route.LogicalOwner.equal(site.Matcher.Enclosing) {
			addProblem(problems, scope, "logical owner is not present in the exact route chain")
		}
		return
	}
	for index := 1; index < len(route.Hops); index++ {
		if !route.Hops[index-1].Callee.equal(route.Hops[index].Site.Enclosing) {
			addProblem(problems, scope, "route hop %d callee does not equal hop %d enclosing function", index-1, index)
		}
	}
	if !route.Hops[len(route.Hops)-1].Callee.equal(site.Matcher.Enclosing) {
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
	case AccessSessionStoreFrontDoor, AccessRawStoreBypass:
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

func validateCoverage(sites []Site, routes []Route, problems *[]string) {
	routeProfiles := make(map[string]map[BuildProfileID]bool, len(sites))
	for _, site := range sites {
		routeProfiles[site.ID] = make(map[BuildProfileID]bool)
	}
	for _, route := range routes {
		profiles := routeProfiles[route.SiteID]
		if profiles == nil {
			continue
		}
		for _, profile := range route.BuildProfiles {
			profiles[profile] = true
		}
	}
	for _, site := range sites {
		for _, profile := range site.BuildProfiles {
			if !routeProfiles[site.ID][profile] {
				*problems = append(*problems, fmt.Sprintf("site %q has no ownership route in build profile %q", site.ID, profile))
			}
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
	}
}

func validateSinkSlot(slot ValueSlot, scope string, problems *[]string) {
	if !oneOf(slot.Kind, SlotReceiver, SlotParameter, SlotChannelElement) {
		addProblem(problems, scope, "invalid target sink slot %q", slot.Kind)
		return
	}
	validateSlotIndex(slot, scope, problems)
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
	case SlotReceiver, SlotChannelElement:
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
		FamilyServerTeardown, FamilyProcessSignal,
	)
}

func knownAccessPath(value AccessPath) bool {
	return oneOf(value, AccessWorkerBoundary, AccessSessionStoreFrontDoor, AccessManagerBypass, AccessProviderBypass, AccessRawStoreBypass, AccessProviderNative, AccessProcessBoundary, AccessDirectProcessBypass, AccessDirectEvent, AccessDirectWake)
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

func stringSet[T ~string](values []T) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[string(value)] = true
	}
	return result
}

func profileSet(values []BuildProfileID) map[BuildProfileID]bool {
	result := make(map[BuildProfileID]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func overlappingProfiles(existing map[BuildProfileID]bool, candidates []BuildProfileID) []BuildProfileID {
	var overlap []BuildProfileID
	for _, candidate := range candidates {
		if existing[candidate] {
			overlap = append(overlap, candidate)
		}
	}
	sort.Slice(overlap, func(i, j int) bool { return overlap[i] < overlap[j] })
	return overlap
}

func oneOf[T comparable](value T, candidates ...T) bool {
	for _, candidate := range candidates {
		if value == candidate {
			return true
		}
	}
	return false
}

func (route Route) semanticKey() string {
	hops := make([]string, len(route.Hops))
	for index, hop := range route.Hops {
		hops[index] = hop.key()
	}
	return strings.Join([]string{
		route.SiteID,
		string(route.ActionFamily),
		string(route.ExecutingProcess),
		route.LogicalOwner.key(),
		route.Target.semanticKey(),
		string(route.AccessPath),
		string(route.Continuation.Locus),
		string(route.Continuation.Completion),
		strings.Join(hops, ","),
	}, "|")
}

func (target TargetRef) semanticKey() string {
	return strings.Join([]string{
		string(target.Kind), target.Sink.key(), string(target.Source),
		target.SourceObject.key(), target.SourceSlot.key(),
	}, "|")
}

func (hop RouteHop) key() string {
	return hop.Site.key() + "|" + string(hop.Dispatch) + "|" + hop.Callee.key()
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
	return function.key() == other.key()
}

func (object ObjectRef) key() string {
	return strings.Join([]string{object.Package, object.Receiver, object.Name}, ".")
}

func (object ObjectRef) zero() bool {
	return object == (ObjectRef{})
}

func (slot ValueSlot) key() string {
	return fmt.Sprintf("%s:%d", slot.Kind, slot.Index)
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
