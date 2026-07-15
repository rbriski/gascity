package effectinventory

// The wake catalog records every production timer, cancellation, signal, and
// named-channel input selected by the executable command graph. A shared
// physical wait has one explicit logical route per process context.
const (
	wakeClassControllerCLI            catalogRouteClassID = "wake/controller-wake/cli"
	wakeClassControllerProviderChild  catalogRouteClassID = "wake/controller-wake/provider-child"
	wakeClassControllerProviderResult catalogRouteClassID = "wake/controller-wake/provider-child/result"
	wakeClassControllerSidecar        catalogRouteClassID = "wake/controller-wake/sidecar-poller"
	wakeClassTimersCLI                catalogRouteClassID = "wake/timers-wake/cli"
	wakeClassTimersProviderChild      catalogRouteClassID = "wake/timers-wake/provider-child"
	wakeClassTimersSidecar            catalogRouteClassID = "wake/timers-wake/sidecar-poller"
)

func knownWakeCatalogClassID(id catalogRouteClassID) bool {
	switch id {
	case wakeClassControllerCLI,
		wakeClassControllerProviderChild,
		wakeClassControllerProviderResult,
		wakeClassControllerSidecar,
		wakeClassTimersCLI,
		wakeClassTimersProviderChild,
		wakeClassTimersSidecar:
		return true
	default:
		return false
	}
}

var wakeCatalogRouteClassSpecs = []wakeCatalogRouteClassSpec{
	{ID: wakeClassControllerCLI, ActionFamily: FamilyControllerWake, ExecutingProcess: ProcessForegroundCLI, TargetShape: wakeTargetBoundarySource},
	{ID: wakeClassControllerProviderChild, ActionFamily: FamilyControllerWake, ExecutingProcess: ProcessProviderChild, TargetShape: wakeTargetBoundarySource},
	{ID: wakeClassControllerProviderResult, ActionFamily: FamilyControllerWake, ExecutingProcess: ProcessProviderChild, TargetShape: wakeTargetResultSource},
	{ID: wakeClassControllerSidecar, ActionFamily: FamilyControllerWake, ExecutingProcess: ProcessSidecarPoller, TargetShape: wakeTargetBoundarySource},
	{ID: wakeClassTimersCLI, ActionFamily: FamilyTimersWake, ExecutingProcess: ProcessForegroundCLI, TargetShape: wakeTargetBoundarySource},
	{ID: wakeClassTimersProviderChild, ActionFamily: FamilyTimersWake, ExecutingProcess: ProcessProviderChild, TargetShape: wakeTargetBoundarySource},
	{ID: wakeClassTimersSidecar, ActionFamily: FamilyTimersWake, ExecutingProcess: ProcessSidecarPoller, TargetShape: wakeTargetBoundarySource},
}

var wakeCatalogSiteSpecs = []wakeCatalogSiteSpec{
	{BoundaryID: "wake.context.done", Operation: OperationSelectReceive, Package: gcCommandPackage, Function: "readWithTimeout", File: "cmd/gc/dolt_cleanup_discovery.go", Ordinal: 1, ProfileSet: wakeProfileSetAll, Classes: []catalogRouteClassID{wakeClassControllerCLI}},
	{BoundaryID: "wake.context.done", Operation: OperationSelectReceive, Package: gcCommandPackage, Function: "runManagedDoltScopeWatchdog", File: "cmd/gc/dolt_scope_watchdog.go", Ordinal: 1, ProfileSet: wakeProfileSetAll, Classes: []catalogRouteClassID{wakeClassControllerProviderChild}},
	{BoundaryID: "wake.context.done", Operation: OperationSelectReceive, Package: gcCommandPackage, Function: "runManagedDoltTestWatchdog", File: "cmd/gc/dolt_start_managed.go", Ordinal: 1, ProfileSet: wakeProfileSetAll, Classes: []catalogRouteClassID{wakeClassControllerProviderChild}},
	{BoundaryID: "wake.context.done", Operation: OperationSelectReceive, Package: "github.com/gastownhall/gascity/internal/productmetrics", Receiver: "unixStorageDirectory", Function: "acquireLock", File: "internal/productmetrics/lock_unix.go", Ordinal: 1, ProfileSet: wakeProfileSetUnix, Classes: []catalogRouteClassID{wakeClassControllerCLI, wakeClassControllerSidecar}, ExplicitAcquireLockRoutes: true},
	{BoundaryID: "wake.context.done", Operation: OperationSelectReceive, Package: "github.com/gastownhall/gascity/internal/productmetrics", Function: "asynchronousUploadStart", File: "internal/productmetrics/spawn.go", ClosurePath: []int{1}, Ordinal: 1, ProfileSet: wakeProfileSetAll, Classes: []catalogRouteClassID{wakeClassControllerSidecar}},
	{BoundaryID: "wake.signal.notify-context", Operation: OperationCall, Package: gcCommandPackage, Function: "runManagedDoltScopeWatchdog", File: "cmd/gc/dolt_scope_watchdog.go", Ordinal: 1, ProfileSet: wakeProfileSetAll, Classes: []catalogRouteClassID{wakeClassControllerProviderResult}},
	{BoundaryID: "wake.signal.notify-context", Operation: OperationCall, Package: gcCommandPackage, Function: "runManagedDoltTestWatchdog", File: "cmd/gc/dolt_start_managed.go", Ordinal: 1, ProfileSet: wakeProfileSetAll, Classes: []catalogRouteClassID{wakeClassControllerProviderResult}},
	{BoundaryID: "wake.time.ticker", Operation: OperationSelectReceive, Package: gcCommandPackage, Function: "runManagedDoltScopeWatchdog", File: "cmd/gc/dolt_scope_watchdog.go", Ordinal: 1, ProfileSet: wakeProfileSetAll, Classes: []catalogRouteClassID{wakeClassTimersProviderChild}},
	{BoundaryID: "wake.time.ticker", Operation: OperationSelectReceive, Package: gcCommandPackage, Function: "runManagedDoltTestWatchdog", File: "cmd/gc/dolt_start_managed.go", Ordinal: 1, ProfileSet: wakeProfileSetAll, Classes: []catalogRouteClassID{wakeClassTimersProviderChild}},
	{BoundaryID: "wake.time.timer", Operation: OperationChannelReceive, Package: "github.com/gastownhall/gascity/internal/productmetrics", Receiver: "unixStorageDirectory", Function: "acquireLock", File: "internal/productmetrics/lock_unix.go", Ordinal: 1, ProfileSet: wakeProfileSetUnix, Classes: []catalogRouteClassID{wakeClassTimersCLI, wakeClassTimersSidecar}, ExplicitAcquireLockRoutes: true},
	{BoundaryID: "wake.time.timer", Operation: OperationSelectReceive, Package: "github.com/gastownhall/gascity/internal/productmetrics", Receiver: "unixStorageDirectory", Function: "acquireLock", File: "internal/productmetrics/lock_unix.go", Ordinal: 1, ProfileSet: wakeProfileSetUnix, Classes: []catalogRouteClassID{wakeClassTimersCLI, wakeClassTimersSidecar}, ExplicitAcquireLockRoutes: true},
}
