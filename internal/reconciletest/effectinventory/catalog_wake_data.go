package effectinventory

// The wake catalog records every production timer, cancellation, signal, and
// named-channel input selected by the executable command graph. A shared
// physical wait has one explicit logical route per process context.
const (
	wakeClassControllerControllerChannel catalogRouteClassID = "wake/controller-wake/controller/controller-channel"
	wakeClassControllerController        catalogRouteClassID = "wake/controller-wake/controller"
	wakeClassControllerAPI               catalogRouteClassID = "wake/controller-wake/api-in-controller"
	wakeClassControllerCLI               catalogRouteClassID = "wake/controller-wake/cli"
	wakeClassControllerCLIResult         catalogRouteClassID = "wake/controller-wake/cli/result"
	wakeClassControllerProviderChild     catalogRouteClassID = "wake/controller-wake/provider-child"
	wakeClassControllerProviderResult    catalogRouteClassID = "wake/controller-wake/provider-child/result"
	wakeClassControllerSidecar           catalogRouteClassID = "wake/controller-wake/sidecar-poller"
	wakeClassTimersCLI                   catalogRouteClassID = "wake/timers-wake/cli"
	wakeClassTimersController            catalogRouteClassID = "wake/timers-wake/controller"
	wakeClassTimersAPI                   catalogRouteClassID = "wake/timers-wake/api-in-controller"
	wakeClassTimersProviderChild         catalogRouteClassID = "wake/timers-wake/provider-child"
	wakeClassTimersSidecar               catalogRouteClassID = "wake/timers-wake/sidecar-poller"
)

func knownWakeCatalogClassID(id catalogRouteClassID) bool {
	switch id {
	case wakeClassControllerControllerChannel,
		wakeClassControllerController,
		wakeClassControllerAPI,
		wakeClassControllerCLI,
		wakeClassControllerCLIResult,
		wakeClassControllerProviderChild,
		wakeClassControllerProviderResult,
		wakeClassControllerSidecar,
		wakeClassTimersCLI,
		wakeClassTimersController,
		wakeClassTimersAPI,
		wakeClassTimersProviderChild,
		wakeClassTimersSidecar:
		return true
	default:
		return false
	}
}

var wakeCatalogRouteClassSpecs = []wakeCatalogRouteClassSpec{
	{ID: wakeClassControllerControllerChannel, ActionFamily: FamilyControllerWake, ExecutingProcess: ProcessController, TargetShape: wakeTargetControllerChannel},
	{ID: wakeClassControllerController, ActionFamily: FamilyControllerWake, ExecutingProcess: ProcessController, TargetShape: wakeTargetBoundarySource},
	{ID: wakeClassControllerAPI, ActionFamily: FamilyControllerWake, ExecutingProcess: ProcessAPIInController, TargetShape: wakeTargetBoundarySource},
	{ID: wakeClassControllerCLI, ActionFamily: FamilyControllerWake, ExecutingProcess: ProcessForegroundCLI, TargetShape: wakeTargetBoundarySource},
	{ID: wakeClassControllerCLIResult, ActionFamily: FamilyControllerWake, ExecutingProcess: ProcessForegroundCLI, TargetShape: wakeTargetResultSource},
	{ID: wakeClassControllerProviderChild, ActionFamily: FamilyControllerWake, ExecutingProcess: ProcessProviderChild, TargetShape: wakeTargetBoundarySource},
	{ID: wakeClassControllerProviderResult, ActionFamily: FamilyControllerWake, ExecutingProcess: ProcessProviderChild, TargetShape: wakeTargetResultSource},
	{ID: wakeClassControllerSidecar, ActionFamily: FamilyControllerWake, ExecutingProcess: ProcessSidecarPoller, TargetShape: wakeTargetBoundarySource},
	{ID: wakeClassTimersCLI, ActionFamily: FamilyTimersWake, ExecutingProcess: ProcessForegroundCLI, TargetShape: wakeTargetBoundarySource},
	{ID: wakeClassTimersController, ActionFamily: FamilyTimersWake, ExecutingProcess: ProcessController, TargetShape: wakeTargetBoundarySource},
	{ID: wakeClassTimersAPI, ActionFamily: FamilyTimersWake, ExecutingProcess: ProcessAPIInController, TargetShape: wakeTargetBoundarySource},
	{ID: wakeClassTimersProviderChild, ActionFamily: FamilyTimersWake, ExecutingProcess: ProcessProviderChild, TargetShape: wakeTargetBoundarySource},
	{ID: wakeClassTimersSidecar, ActionFamily: FamilyTimersWake, ExecutingProcess: ProcessSidecarPoller, TargetShape: wakeTargetBoundarySource},
}

var wakeCatalogSeedSiteSpecs = []wakeCatalogSiteSpec{
	{BoundaryID: "wake.context.done", Operation: OperationSelectReceive, Package: gcCommandPackage, Function: "readWithTimeout", File: "cmd/gc/dolt_cleanup_discovery.go", Ordinal: 1, ProfileSet: wakeProfileSetAll, Classes: []catalogRouteClassID{wakeClassControllerCLI}},
	{BoundaryID: "wake.context.done", Operation: OperationSelectReceive, Package: gcCommandPackage, Function: "runManagedDoltScopeWatchdog", File: "cmd/gc/dolt_scope_watchdog.go", Ordinal: 1, ProfileSet: wakeProfileSetAll, Classes: []catalogRouteClassID{wakeClassControllerProviderChild}},
	{BoundaryID: "wake.context.done", Operation: OperationSelectReceive, Package: gcCommandPackage, Function: "runManagedDoltTestWatchdog", File: "cmd/gc/dolt_start_managed.go", Ordinal: 1, ProfileSet: wakeProfileSetAll, Classes: []catalogRouteClassID{wakeClassControllerProviderChild}},
	{BoundaryID: "wake.context.done", Operation: OperationSelectReceive, Package: "github.com/gastownhall/gascity/internal/productmetrics", Receiver: "unixStorageDirectory", Function: "acquireLock", File: "internal/productmetrics/lock_unix.go", Ordinal: 1, ProfileSet: wakeProfileSetUnix, ExplicitRoutes: wakeAcquireLockExplicitRoutes(wakeClassControllerCLI, wakeClassControllerSidecar)},
	{BoundaryID: "wake.context.done", Operation: OperationSelectReceive, Package: "github.com/gastownhall/gascity/internal/productmetrics", Function: "asynchronousUploadStart", File: "internal/productmetrics/spawn.go", ClosurePath: []int{1}, Ordinal: 1, ProfileSet: wakeProfileSetAll, Classes: []catalogRouteClassID{wakeClassControllerSidecar}},
	{BoundaryID: "wake.signal.notify-context", Operation: OperationCall, Package: gcCommandPackage, Function: "runManagedDoltScopeWatchdog", File: "cmd/gc/dolt_scope_watchdog.go", Ordinal: 1, ProfileSet: wakeProfileSetAll, Classes: []catalogRouteClassID{wakeClassControllerProviderResult}},
	{BoundaryID: "wake.signal.notify-context", Operation: OperationCall, Package: gcCommandPackage, Function: "runManagedDoltTestWatchdog", File: "cmd/gc/dolt_start_managed.go", Ordinal: 1, ProfileSet: wakeProfileSetAll, Classes: []catalogRouteClassID{wakeClassControllerProviderResult}},
	{BoundaryID: "wake.time.ticker", Operation: OperationSelectReceive, Package: gcCommandPackage, Function: "runManagedDoltScopeWatchdog", File: "cmd/gc/dolt_scope_watchdog.go", Ordinal: 1, ProfileSet: wakeProfileSetAll, Classes: []catalogRouteClassID{wakeClassTimersProviderChild}},
	{BoundaryID: "wake.time.ticker", Operation: OperationSelectReceive, Package: gcCommandPackage, Function: "runManagedDoltTestWatchdog", File: "cmd/gc/dolt_start_managed.go", Ordinal: 1, ProfileSet: wakeProfileSetAll, Classes: []catalogRouteClassID{wakeClassTimersProviderChild}},
	{BoundaryID: "wake.time.timer", Operation: OperationChannelReceive, Package: "github.com/gastownhall/gascity/internal/productmetrics", Receiver: "unixStorageDirectory", Function: "acquireLock", File: "internal/productmetrics/lock_unix.go", Ordinal: 1, ProfileSet: wakeProfileSetUnix, ExplicitRoutes: wakeAcquireLockExplicitRoutes(wakeClassTimersCLI, wakeClassTimersSidecar)},
	{BoundaryID: "wake.time.timer", Operation: OperationSelectReceive, Package: "github.com/gastownhall/gascity/internal/productmetrics", Receiver: "unixStorageDirectory", Function: "acquireLock", File: "internal/productmetrics/lock_unix.go", Ordinal: 1, ProfileSet: wakeProfileSetUnix, ExplicitRoutes: wakeAcquireLockExplicitRoutes(wakeClassTimersCLI, wakeClassTimersSidecar)},
}
