package effectinventory

const (
	beadsPackage        = "github.com/gastownhall/gascity/internal/beads"
	eventsPackage       = "github.com/gastownhall/gascity/internal/events"
	pidutilPackage      = "github.com/gastownhall/gascity/internal/pidutil"
	processgroupPackage = "github.com/gastownhall/gascity/internal/processgroup"
	proctablePackage    = "github.com/gastownhall/gascity/internal/runtime/proctable"
	runtimePackage      = "github.com/gastownhall/gascity/internal/runtime"
	workspacesvcPackage = "github.com/gastownhall/gascity/internal/workspacesvc"
	gcCommandPackage    = "github.com/gastownhall/gascity/cmd/gc"
)

// CanonicalBoundaries returns the closed typed vocabulary used to discover
// current reconciler effects. It returns a fresh slice on every call.
func CanonicalBoundaries() []BoundaryDefinition {
	definitions := canonicalBoundaries()
	return append([]BoundaryDefinition(nil), definitions...)
}

func canonicalBoundaries() []BoundaryDefinition {
	var definitions []BoundaryDefinition
	addInterface := func(id string, kind EffectKind, packagePath, receiver string, names ...string) {
		for _, name := range names {
			definitions = append(definitions, BoundaryDefinition{
				ID:     id + "." + name,
				Kind:   kind,
				Object: ObjectRef{Package: packagePath, Receiver: receiver, Name: name},
				Match:  ObjectMatchInterfaceImplementors,
			})
		}
	}
	addExact := func(id string, kind EffectKind, packagePath, receiver string, names ...string) {
		for _, name := range names {
			definitions = append(definitions, BoundaryDefinition{
				ID:     id + "." + name,
				Kind:   kind,
				Object: ObjectRef{Package: packagePath, Receiver: receiver, Name: name},
				Match:  ObjectMatchExact,
			})
		}
	}
	addChannelField := func(id, packagePath, receiver, name string) {
		definitions = append(definitions, BoundaryDefinition{
			ID:     id,
			Kind:   KindWakeSource,
			Object: ObjectRef{Package: packagePath, Receiver: receiver, Name: name},
			Match:  ObjectMatchChannel,
		})
	}
	addChannelResult := func(id, packagePath, receiver, name string, result int) {
		definitions = append(definitions, BoundaryDefinition{
			ID:     id,
			Kind:   KindWakeSource,
			Object: ObjectRef{Package: packagePath, Receiver: receiver, Name: name},
			Match:  ObjectMatchChannel,
			Output: ValueSlot{Kind: SlotResult, Index: result},
		})
	}
	addChannelInput := func(id, packagePath, receiver, name string, parameter int) {
		definitions = append(definitions, BoundaryDefinition{
			ID:     id,
			Kind:   KindWakeSource,
			Object: ObjectRef{Package: packagePath, Receiver: receiver, Name: name},
			Match:  ObjectMatchChannel,
			Input:  ValueSlot{Kind: SlotParameter, Index: parameter},
		})
	}

	// Writer is the narrow canonical mutation handle. Store implements Writer,
	// so registering Store's duplicate method set would classify one typed call
	// against two boundaries and miss direct StoreHandles.Writer calls.
	addInterface("beads.writer", KindStoreMutation, beadsPackage, "Writer",
		"Create", "Update", "Close", "Reopen", "CloseAll", "SetMetadata",
		"SetMetadataBatch", "Delete", "DepAdd", "DepRemove")
	addInterface("beads.store", KindStoreMutation, beadsPackage, "Store", "Tx")
	addInterface("beads.conditional-assignment", KindStoreMutation, beadsPackage, "ConditionalAssignmentReleaser", "ReleaseIfCurrent")
	addInterface("beads.batch-delete", KindStoreMutation, beadsPackage, "BatchDeleter", "DeleteBatch")
	addInterface("beads.graph-apply", KindStoreMutation, beadsPackage, "GraphApplyStore", "ApplyGraphPlan")
	addInterface("beads.storage-graph-apply", KindStoreMutation, beadsPackage, "StorageGraphApplyStore", "ApplyGraphPlanWithStorage")
	addInterface("beads.storage-create", KindStoreMutation, beadsPackage, "StorageCreateStore", "CreateWithStorage")
	addInterface("gc.explicit-reason-close", KindStoreMutation, gcCommandPackage, "explicitReasonCloser", "CloseWithReason")
	addExact("beads.bd-store", KindStoreMutation, beadsPackage, "BdStore", "Claim")

	// Legacy Provider remains a real production facade while the deconflated
	// Runtime/Place/Transport/Attachment seams migrate independently.
	addInterface("runtime.provider", KindProviderMutation, runtimePackage, "Provider",
		"Start", "Stop", "Interrupt", "Attach", "Nudge", "ClearScrollback",
		"CopyTo", "SendKeys", "RunLive")
	addInterface("runtime.meta-store", KindProviderMutation, runtimePackage, "MetaStore", "SetMeta", "RemoveMeta")
	addInterface("runtime.runtime", KindProviderMutation, runtimePackage, "Runtime", "Provision", "Teardown")
	// Place.Exec is the typed outer arbitrary-command mutation. ExecProvider.Exec
	// is intentionally absent: providers also use that inner connection for
	// read-only ProcessAlive probes, while mutating calls enter through Provider,
	// Place, Carrier, or Attachment.
	addInterface("runtime.place", KindProviderMutation, runtimePackage, "Place", "Exec", "Stage", "Teardown")
	addInterface("runtime.transport", KindProviderMutation, runtimePackage, "Transport", "Launch", "Attach")
	addInterface("runtime.attachment", KindProviderMutation, runtimePackage, "Attachment",
		"Nudge", "SendKeys", "Interrupt", "ClearScrollback", "Close")
	addInterface("runtime.carrier", KindProviderMutation, runtimePackage, "Carrier",
		"Nudge", "SendKeys", "Interrupt", "ClearScrollback")
	addInterface("runtime.interaction", KindProviderMutation, runtimePackage, "InteractionProvider", "Respond")
	addInterface("runtime.dialog", KindProviderMutation, runtimePackage, "DialogProvider", "DismissKnownDialogs")
	addInterface("runtime.immediate-nudge", KindProviderMutation, runtimePackage, "ImmediateNudgeProvider", "NudgeNow")
	addInterface("runtime.interrupted-turn-reset", KindProviderMutation, runtimePackage, "InterruptedTurnResetProvider", "ResetInterruptedTurn")
	addInterface("runtime.relaunch", KindProviderMutation, runtimePackage, "RelaunchProvider", "Relaunch")
	addInterface("runtime.process-table", KindProviderMutation, runtimePackage, "ProcessTableScanner", "TerminateRuntime")
	addInterface("runtime.server-lifecycle", KindProviderMutation, runtimePackage, "ServerLifecycleProvider", "ConfigureServer", "TeardownServer")

	addInterface("events.recorder", KindEventEmission, eventsPackage, "Recorder", "Record")

	// Portable process seams remain the auditable boundary across OS-specific
	// implementations. Direct os.Process calls in the production dependency
	// closure are explicit residual leaves.
	addExact("pidutil", KindProcessMutation, pidutilPackage, "", "Signal", "SignalProcess")
	addExact("processgroup", KindProcessMutation, processgroupPackage, "", "SignalGroup", "SignalCommand", "Terminate", "TerminateCommand")
	addExact("runtime.process", KindProcessMutation, runtimePackage, "", "SignalProcessGroup", "TerminateManagedProcess")
	addExact("runtime.proctable", KindProcessMutation, proctablePackage, "", "KillByPID")
	addExact("os.process", KindProcessMutation, "os", "Process", "Kill")

	// Workspace services execute inside the controller and own child processes.
	// Their controller entry points are explicit effect vehicles in addition to
	// the lower-level process mutations found in their production bodies.
	addExact("workspacesvc.manager", KindProcessMutation, workspacesvcPackage, "Manager", "Reload", "Tick", "Close")
	addInterface("workspacesvc.registry", KindProcessMutation, workspacesvcPackage, "Registry", "Restart")

	// Controller channels are named at their owning fields so every producer
	// and consumer shares one identity even when helpers pass the channel as a
	// parameter. Standard timer, cancellation, and signal registrations cover
	// external/time-based wake inputs throughout the production dependency
	// closure.
	for _, field := range []string{"pokeCh", "controlDispatcherCh", "nudgeWakeCh", "convergenceReqCh", "reloadReqCh"} {
		addChannelField("wake.city-runtime."+field, gcCommandPackage, "CityRuntime", field)
	}
	addChannelField("wake.tick-debouncer.fire", gcCommandPackage, "tickDebouncer", "fireCh")
	addChannelField("wake.time.ticker", "time", "Ticker", "C")
	addChannelField("wake.time.timer", "time", "Timer", "C")
	addChannelResult("wake.time.after", "time", "", "After", 1)
	addChannelResult("wake.context.done", "context", "Context", "Done", 1)
	addChannelInput("wake.signal.notify", "os/signal", "", "Notify", 1)
	definitions = append(definitions, BoundaryDefinition{
		ID:     "wake.signal.notify-context",
		Kind:   KindWakeSource,
		Object: ObjectRef{Package: "os/signal", Name: "NotifyContext"},
		Match:  ObjectMatchExact,
	})

	return definitions
}
