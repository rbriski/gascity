package effectinventory

// CanonicalBoundaries returns the canonical reconciler effect-boundary seed set.
// It is the single exported accessor for the discovery vocabulary, shared by the
// analyzer golden gate and the P0.1 ownership registry so both reason about the
// same boundaries.
func CanonicalBoundaries() []BoundaryDefinition { return canonicalBoundaries() }

// canonicalBoundaries returns the closed set of reconciler effect-boundary
// seeds for the P0.1 effect inventory. Each seed names one boundary method (or
// process primitive) and how the analyzer resolves it against typed calls.
//
// Interface boundaries (beads.Store, runtime.Provider, the optional runtime
// provider extensions, and events.Recorder) use
// ObjectMatchInterfaceImplementors: a direct call matches when its selected
// method name equals Object.Name AND the receiver's static type implements the
// named interface. Process primitives use ObjectMatchExact (a package function
// for syscall.Kill; a named-type method for the os.Process verbs).
//
// The set is 31 seeds: 26 core (11 store + 10 provider + 4 process + 1 event)
// plus the 5 optional runtime provider extensions, which are seeded as
// KindProviderMutation.
func canonicalBoundaries() []BoundaryDefinition {
	const (
		beadsPkg   = "github.com/gastownhall/gascity/internal/beads"
		runtimePkg = "github.com/gastownhall/gascity/internal/runtime"
		eventsPkg  = "github.com/gastownhall/gascity/internal/events"
	)

	iface := func(id string, kind EffectKind, pkg, recv, method string) BoundaryDefinition {
		return BoundaryDefinition{
			ID:     id,
			Kind:   kind,
			Object: ObjectRef{Package: pkg, Receiver: recv, Name: method},
			Match:  ObjectMatchInterfaceImplementors,
		}
	}
	exact := func(id, pkg, recv, name string) BoundaryDefinition {
		return BoundaryDefinition{
			ID:     id,
			Kind:   KindProcessMutation,
			Object: ObjectRef{Package: pkg, Receiver: recv, Name: name},
			Match:  ObjectMatchExact,
		}
	}

	var seeds []BoundaryDefinition

	// Store mutations — beads.Store (11).
	for _, m := range []string{
		"Create", "Update", "Close", "Reopen", "CloseAll",
		"SetMetadata", "SetMetadataBatch", "Tx", "Delete", "DepAdd", "DepRemove",
	} {
		seeds = append(seeds, iface("store."+m, KindStoreMutation, beadsPkg, "Store", m))
	}

	// Provider mutations — runtime.Provider (10).
	for _, m := range []string{
		"Start", "Stop", "Interrupt", "Nudge", "SetMeta", "RemoveMeta",
		"ClearScrollback", "CopyTo", "SendKeys", "RunLive",
	} {
		seeds = append(seeds, iface("provider."+m, KindProviderMutation, runtimePkg, "Provider", m))
	}

	// Optional runtime provider extensions — mutating verbs only (5). Seeded as
	// provider mutations per the P0.1 boundary decision.
	seeds = append(seeds,
		iface("exec.Exec", KindProviderMutation, runtimePkg, "ExecProvider", "Exec"),
		iface("relaunch.Relaunch", KindProviderMutation, runtimePkg, "RelaunchProvider", "Relaunch"),
		iface("interaction.Respond", KindProviderMutation, runtimePkg, "InteractionProvider", "Respond"),
		iface("immediatenudge.NudgeNow", KindProviderMutation, runtimePkg, "ImmediateNudgeProvider", "NudgeNow"),
		iface("dialog.DismissKnownDialogs", KindProviderMutation, runtimePkg, "DialogProvider", "DismissKnownDialogs"),
	)

	// Event emission — events.Recorder (1). events.Provider embeds Recorder, so
	// calls on the fuller bus interface still resolve here.
	seeds = append(seeds, iface("events.Record", KindEventEmission, eventsPkg, "Recorder", "Record"))

	// Process mutations — exact primitives (4). syscall.Kill is a package
	// function (no receiver); the os.Process verbs are methods on the named type.
	seeds = append(seeds,
		BoundaryDefinition{ID: "syscall.Kill", Kind: KindProcessMutation, Object: ObjectRef{Package: "syscall", Name: "Kill"}, Match: ObjectMatchExact},
		exact("os.process.Signal", "os", "Process", "Signal"),
		exact("os.process.Kill", "os", "Process", "Kill"),
		exact("os.process.Release", "os", "Process", "Release"),
	)

	return seeds
}
