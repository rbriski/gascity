package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

func workerSessionCatalogWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City) (*worker.SessionCatalog, error) {
	factory, err := workerFactoryWithConfig(cityPath, store, sp, cfg)
	if err != nil {
		return nil, err
	}
	return factory.Catalog()
}

func workerFactoryWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City) (*worker.Factory, error) {
	var (
		resolveTransport func(template string) string
		searchPaths      []string
	)
	if cfg != nil {
		rigContext := currentRigContext(cfg)
		resolveTransport = func(template string) string {
			agentCfg, ok := resolveAgentIdentity(cfg, template, rigContext)
			if !ok {
				return ""
			}
			return agentCfg.Session
		}
		searchPaths = worker.MergeSearchPaths(cfg.Daemon.ObservePaths)
	}
	return worker.NewFactory(worker.FactoryConfig{
		Store:                 store,
		Provider:              sp,
		CityPath:              cityPath,
		SearchPaths:           searchPaths,
		ResolveTransport:      resolveTransport,
		ResolveSessionRuntime: workerSessionRuntimeResolverWithConfig(cityPath, cfg),
	})
}

func workerSessionRuntimeResolverWithConfig(cityPath string, cfg *config.City) worker.SessionRuntimeResolver {
	if cfg == nil {
		return nil
	}
	return func(info session.Info, sessionKind string) (*worker.ResolvedRuntime, error) {
		runtimeCfg := resolvedWorkerRuntimeWithConfig(cityPath, cfg, info, sessionKind)
		if runtimeCfg == nil {
			return nil, nil
		}
		normalized, err := worker.NormalizeResolvedRuntime(*runtimeCfg)
		if err != nil {
			return nil, err
		}
		return &normalized, nil
	}
}

func workerSessionCreateHints(resolved *config.ResolvedProvider) runtime.Config {
	if resolved == nil {
		return runtime.Config{}
	}
	return runtime.Config{
		ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
		ReadyDelayMs:           resolved.ReadyDelayMs,
		ProcessNames:           resolved.ProcessNames,
		EmitsPermissionWarning: resolved.EmitsPermissionWarning,
	}
}

func newWorkerSessionHandleForResolvedRuntimeWithConfig(
	cityPath string,
	store beads.Store,
	sp runtime.Provider,
	cfg *config.City,
	alias, explicitName, template, title, command, provider, workDir, transport string,
	resolved *config.ResolvedProvider,
	metadata map[string]string,
) (worker.Handle, error) {
	factory, err := workerFactoryWithConfig(cityPath, store, sp, cfg)
	if err != nil {
		return nil, err
	}
	sessionCfg, err := resolvedWorkerSessionConfigWithConfig(
		command,
		provider,
		workDir,
		alias,
		explicitName,
		template,
		title,
		transport,
		resolved,
		metadata,
	)
	if err != nil {
		return nil, err
	}
	return factory.SessionForResolvedRuntime(sessionCfg)
}

func resolvedWorkerSessionConfigWithConfig(
	command string,
	provider string,
	workDir string,
	alias string,
	explicitName string,
	template string,
	title string,
	transport string,
	resolved *config.ResolvedProvider,
	metadata map[string]string,
) (worker.ResolvedSessionConfig, error) {
	if resolved == nil {
		return worker.ResolvedSessionConfig{}, fmt.Errorf("resolved provider is required")
	}
	command = strings.TrimSpace(command)
	if command == "" {
		command = strings.TrimSpace(resolved.CommandString())
	}
	providerName := strings.TrimSpace(resolved.Name)
	if providerName == "" {
		providerName = strings.TrimSpace(provider)
	}
	if command == "" {
		command = providerName
	}
	return worker.NormalizeResolvedSessionConfig(worker.ResolvedSessionConfig{
		Alias:        alias,
		ExplicitName: explicitName,
		Template:     template,
		Title:        title,
		Transport:    transport,
		Metadata:     metadata,
		Runtime: worker.ResolvedRuntime{
			Command:    command,
			WorkDir:    workDir,
			Provider:   providerName,
			SessionEnv: resolved.Env,
			Resume: session.ProviderResume{
				ResumeFlag:    resolved.ResumeFlag,
				ResumeStyle:   resolved.ResumeStyle,
				ResumeCommand: resolved.ResumeCommand,
				SessionIDFlag: resolved.SessionIDFlag,
			},
			Hints: workerSessionCreateHints(resolved),
		},
	})
}

func workerHandleForSessionWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, id string) (worker.Handle, error) {
	factory, err := workerFactoryWithConfig(cityPath, store, sp, cfg)
	if err != nil {
		return nil, err
	}
	return factory.SessionByID(id)
}

func workerHandleForSessionTargetWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string) (worker.Handle, error) {
	return workerHandleForSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, target, nil)
}

func workerHandleForSessionTargetWithRuntimeHintsWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string, processNames []string) (worker.Handle, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, session.ErrSessionNotFound
	}
	factory, err := workerFactoryWithConfig(cityPath, store, sp, cfg)
	if err != nil {
		return nil, err
	}
	if store != nil {
		if id, err := session.ResolveSessionIDByExactID(store, target); err == nil {
			return factory.SessionByID(id)
		}
		if id, err := session.ResolveSessionID(store, target); err == nil {
			return factory.SessionByID(id)
		}
		if sp != nil {
			if sessionID, metaErr := sp.GetMeta(target, "GC_SESSION_ID"); metaErr == nil && strings.TrimSpace(sessionID) != "" {
				return factory.SessionByID(strings.TrimSpace(sessionID))
			}
		}
	}
	if sp == nil {
		return nil, session.ErrSessionNotFound
	}
	providerName := target
	if liveProvider, err := sp.GetMeta(target, "GC_PROVIDER"); err == nil && strings.TrimSpace(liveProvider) != "" {
		providerName = strings.TrimSpace(liveProvider)
	}
	return factory.RuntimeHandle(target, providerName, "", processNames)
}

func runtimeWorkerHandleWithConfig(
	cityPath string,
	store beads.Store,
	sp runtime.Provider,
	cfg *config.City,
	sessionName string,
	providerName string,
	transport string,
	processNames []string,
) (worker.Handle, error) {
	factory, err := workerFactoryWithConfig(cityPath, store, sp, cfg)
	if err != nil {
		return nil, err
	}
	return factory.RuntimeHandle(sessionName, providerName, transport, processNames)
}

func workerKillSessionTargetWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string) error {
	handle, err := workerHandleForSessionTargetWithConfig(cityPath, store, sp, cfg, target)
	if err != nil {
		return err
	}
	return handle.Kill(context.Background())
}

func workerStopSessionTargetWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string) error {
	handle, err := workerHandleForSessionTargetWithConfig(cityPath, store, sp, cfg, target)
	if err != nil {
		return err
	}
	return handle.Stop(context.Background())
}

func workerInterruptSessionTargetWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string) error {
	handle, err := workerHandleForSessionTargetWithConfig(cityPath, store, sp, cfg, target)
	if err != nil {
		return err
	}
	return handle.Interrupt(context.Background(), worker.InterruptRequest{})
}

func workerObserveSessionTargetWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string) (worker.LiveObservation, error) {
	return workerObserveSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, target, nil)
}

func workerObserveSessionTargetWithRuntimeHintsWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string, processNames []string) (worker.LiveObservation, error) {
	handle, err := workerHandleForSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, target, processNames)
	if err != nil {
		return worker.LiveObservation{}, err
	}
	return worker.ObserveHandle(context.Background(), handle)
}

// workerObserveSessionTargetWithPrefetchedSessions is
// workerObserveSessionTargetWithConfig with an injected slice of session
// beads. When sessionBeads is non-nil, session-name and alias resolution
// is served from the slice instead of calling store.List — turning N agent
// observations into 1 list plus N linear scans. Used by CLI read-models
// (gc status, gc rig status) that enumerate all agents. The slice should
// already be filtered to open sessions (IncludeClosed: false) — same as
// ResolveSessionID's internal query. See bead ga-jwtz.
func workerObserveSessionTargetWithPrefetchedSessions(
	cityPath string,
	store beads.Store,
	sp runtime.Provider,
	cfg *config.City,
	target string,
	sessionBeads []beads.Bead,
) (worker.LiveObservation, error) {
	handle, err := workerHandleForSessionTargetWithPrefetched(cityPath, store, sp, cfg, target, nil, sessionBeads)
	if err != nil {
		return worker.LiveObservation{}, err
	}
	return worker.ObserveHandle(context.Background(), handle)
}

func workerHandleForSessionTargetWithPrefetched(
	cityPath string,
	store beads.Store,
	sp runtime.Provider,
	cfg *config.City,
	target string,
	processNames []string,
	sessionBeads []beads.Bead,
) (worker.Handle, error) {
	if sessionBeads == nil {
		return workerHandleForSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, target, processNames)
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, session.ErrSessionNotFound
	}
	factory, err := workerFactoryWithConfig(cityPath, store, sp, cfg)
	if err != nil {
		return nil, err
	}
	if id, ok := resolveSessionIDFromPrefetched(target, sessionBeads); ok {
		return factory.SessionByID(id)
	}
	if sp != nil {
		if sessionID, metaErr := sp.GetMeta(target, "GC_SESSION_ID"); metaErr == nil && strings.TrimSpace(sessionID) != "" {
			return factory.SessionByID(strings.TrimSpace(sessionID))
		}
	}
	if sp == nil {
		return nil, session.ErrSessionNotFound
	}
	providerName := target
	if liveProvider, err := sp.GetMeta(target, "GC_PROVIDER"); err == nil && strings.TrimSpace(liveProvider) != "" {
		providerName = strings.TrimSpace(liveProvider)
	}
	return factory.RuntimeHandle(target, providerName, "", processNames)
}

// resolveSessionIDFromPrefetched mirrors session.ResolveSessionIDByExactID +
// session.ResolveSessionID against a pre-fetched slice of open session beads.
// Ambiguous matches collapse to "not found from prefetched"; the caller then
// consults the runtime provider, which for status observations correctly
// reports the target as stopped when no unique session can be attributed.
func resolveSessionIDFromPrefetched(target string, sessionBeads []beads.Bead) (string, bool) {
	var openNameMatches []string
	var openAliasMatches []string
	for _, b := range sessionBeads {
		if !session.IsSessionBeadOrRepairable(b) {
			continue
		}
		if b.ID == target {
			return b.ID, true
		}
		if b.Status == "closed" {
			continue
		}
		if strings.TrimSpace(b.Metadata["session_name"]) == target {
			openNameMatches = append(openNameMatches, b.ID)
		}
		if strings.TrimSpace(b.Metadata["alias"]) == target {
			openAliasMatches = append(openAliasMatches, b.ID)
		}
	}
	if len(openNameMatches) == 1 {
		return openNameMatches[0], true
	}
	if len(openAliasMatches) == 1 {
		return openAliasMatches[0], true
	}
	return "", false
}

func workerSessionTargetRunningWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string) (bool, error) {
	obs, err := workerObserveSessionTargetWithConfig(cityPath, store, sp, cfg, target)
	if err != nil {
		return false, err
	}
	return obs.Running, nil
}

func workerSessionTargetAliveWithConfig(store beads.Store, sp runtime.Provider, cfg *config.City, target string, processNames []string) (bool, error) {
	obs, err := workerObserveSessionTargetWithRuntimeHintsWithConfig("", store, sp, cfg, target, processNames)
	if err != nil {
		return false, err
	}
	return obs.Alive, nil
}

func workerSessionTargetAttachedWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string) (bool, error) {
	obs, err := workerObserveSessionTargetWithConfig(cityPath, store, sp, cfg, target)
	if err != nil {
		return false, err
	}
	return obs.Attached, nil
}

func workerSessionTargetLastActivityWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string) (time.Time, error) {
	obs, err := workerObserveSessionTargetWithConfig(cityPath, store, sp, cfg, target)
	if err != nil {
		return time.Time{}, err
	}
	if obs.LastActivity == nil {
		return time.Time{}, nil
	}
	return *obs.LastActivity, nil
}

func workerSessionTargetPeekWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string, lines int, processNames []string) (string, error) {
	handle, err := workerHandleForSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, target, processNames)
	if err != nil {
		return "", err
	}
	return handle.Peek(context.Background(), lines)
}

func workerSessionTargetPendingWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string) (*worker.PendingInteraction, error) {
	handle, err := workerHandleForSessionTargetWithConfig(cityPath, store, sp, cfg, target)
	if err != nil {
		return nil, err
	}
	return handle.Pending(context.Background())
}

func workerRespondSessionTargetWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string, response worker.InteractionResponse) error {
	handle, err := workerHandleForSessionTargetWithConfig(cityPath, store, sp, cfg, target)
	if err != nil {
		return err
	}
	return handle.Respond(context.Background(), response)
}

func resolvedWorkerRuntimeWithConfig(cityPath string, cfg *config.City, info session.Info, sessionKind string) *worker.ResolvedRuntime {
	if cfg == nil {
		return nil
	}
	resolved := resolveWorkerRuntimeWithConfig(cfg, info, sessionKind)
	if resolved == nil {
		return nil
	}

	command := strings.TrimSpace(info.Command)
	if !shouldPreserveStoredRuntimeCommand(command, resolved.CommandString()) {
		launchCommand, err := config.BuildProviderLaunchCommand(cityPath, resolved, nil)
		command = resolved.CommandString()
		if err == nil {
			command = launchCommand.Command
		}
	}
	command = firstNonEmptyGCString(command, info.Provider, resolved.Name)

	workDir := strings.TrimSpace(info.WorkDir)
	if workDir == "" {
		workDir = cityPath
	}
	return &worker.ResolvedRuntime{
		Command:    command,
		WorkDir:    workDir,
		Provider:   firstNonEmptyGCString(info.Provider, resolved.Name),
		SessionEnv: resolved.Env,
		Hints: runtime.Config{
			WorkDir:                workDir,
			ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
			ReadyDelayMs:           resolved.ReadyDelayMs,
			ProcessNames:           resolved.ProcessNames,
			EmitsPermissionWarning: resolved.EmitsPermissionWarning,
		},
		Resume: session.ProviderResume{
			ResumeFlag:    firstNonEmptyGCString(resolved.ResumeFlag, info.ResumeFlag),
			ResumeStyle:   firstNonEmptyGCString(resolved.ResumeStyle, info.ResumeStyle),
			ResumeCommand: firstNonEmptyGCString(resolved.ResumeCommand, info.ResumeCommand),
			SessionIDFlag: resolved.SessionIDFlag,
		},
	}
}

func shouldPreserveStoredRuntimeCommand(storedCommand, resolvedCommand string) bool {
	storedCommand = strings.TrimSpace(storedCommand)
	if storedCommand == "" {
		return false
	}
	resolvedCommand = strings.TrimSpace(resolvedCommand)
	if resolvedCommand == "" {
		return true
	}
	// A bare stored command (just the provider binary) lacks schema
	// defaults like --dangerously-skip-permissions and the --settings
	// path. Rebuild from the current config instead of preserving it.
	// See #799: pool-agent sessions resumed through the control-
	// dispatcher path wedged on interactive permission prompts because
	// the bare stored command was preserved without re-injecting flags.
	if storedCommand == resolvedCommand {
		return false
	}
	return strings.HasPrefix(storedCommand, resolvedCommand+" ")
}

func resolveWorkerRuntimeWithConfig(cfg *config.City, info session.Info, sessionKind string) *config.ResolvedProvider {
	if cfg == nil {
		return nil
	}
	if sessionKind != "provider" {
		if found, ok := resolveAgentIdentity(cfg, info.Template, ""); ok {
			if resolved, err := config.ResolveProvider(&found, &cfg.Workspace, cfg.Providers, exec.LookPath); err == nil {
				return resolved
			}
		}
	}
	resolved, err := config.ResolveProvider(&config.Agent{Provider: info.Template}, &cfg.Workspace, cfg.Providers, exec.LookPath)
	if err != nil {
		return nil
	}
	return resolved
}

func workerDeliveryIntentForSubmitIntent(intent session.SubmitIntent) worker.DeliveryIntent {
	switch intent {
	case session.SubmitIntentFollowUp:
		return worker.DeliveryIntentFollowUp
	case session.SubmitIntentInterruptNow:
		return worker.DeliveryIntentInterruptNow
	default:
		return worker.DeliveryIntentDefault
	}
}

func workerNudgeDeliveryForMode(mode nudgeDeliveryMode) (worker.NudgeDelivery, bool) {
	switch mode {
	case nudgeDeliveryImmediate:
		return worker.NudgeDeliveryImmediate, true
	case nudgeDeliveryWaitIdle:
		return worker.NudgeDeliveryWaitIdle, true
	default:
		return "", false
	}
}

func firstNonEmptyGCString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
