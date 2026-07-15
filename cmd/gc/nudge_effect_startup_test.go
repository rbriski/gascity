package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSelectNudgeEffectOwnershipOffIsZeroCostLegacy(t *testing.T) {
	t.Parallel()
	for name, flags := range map[string]rollout.Flags{
		"explicit off": rollout.ForTest(rollout.WithNudgeEffectOwner(rollout.Off)),
		"zero flags":   {},
	} {
		name, flags := name, flags
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			called := false
			selection, err := selectNudgeEffectOwnership(
				context.Background(),
				flags,
				"not-a-profile",
				func(context.Context) nudgeEffectStartupCapabilities {
					called = true
					return completeNudgeEffectStartupCapabilities()
				},
			)
			if err != nil {
				t.Fatalf("selectNudgeEffectOwnership off: %v", err)
			}
			if called {
				t.Fatal("off inspected startup capabilities")
			}
			if selection.Ownership != nudgeEffectOwnershipLegacy {
				t.Fatalf("off ownership = %d, want legacy", selection.Ownership)
			}
			if selection.Notice != "" {
				t.Fatalf("off notice = %q, want byte-identical silence", selection.Notice)
			}
		})
	}
}

func TestSelectNudgeEffectOwnershipLocalCompleteSelectsKeyedAndWarns(t *testing.T) {
	t.Parallel()
	for _, mode := range []rollout.Mode{rollout.Auto, rollout.Require} {
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			selection, err := selectNudgeEffectOwnership(
				context.Background(),
				rollout.ForTest(rollout.WithNudgeEffectOwner(mode)),
				string(nudgequeue.CommandSecurityProfileStoreWriterIsController),
				func(context.Context) nudgeEffectStartupCapabilities {
					return completeNudgeEffectStartupCapabilities()
				},
			)
			if err != nil {
				t.Fatalf("selectNudgeEffectOwnership %s: %v", mode, err)
			}
			if selection.Ownership != nudgeEffectOwnershipKeyed {
				t.Fatalf("%s ownership = %d, want keyed", mode, selection.Ownership)
			}
			warning := strings.ToLower(selection.Notice)
			for _, want := range []string{"local single-tenant", "store credential", "full session-control authority"} {
				if !strings.Contains(warning, want) {
					t.Fatalf("%s notice %q does not contain %q", mode, selection.Notice, want)
				}
			}
		})
	}
}

func TestSelectNudgeEffectOwnershipMissingDependencyHonorsAutoAndRequire(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		wantReason string
		remove     func(*nudgeEffectStartupCapabilities)
	}{
		"supervisor dispatcher": {
			wantReason: "supervisor nudge dispatcher",
			remove:     func(c *nudgeEffectStartupCapabilities) { c.SupervisorDispatcher = false },
		},
		"atomic repository": {
			wantReason: "atomic command repository",
			remove:     func(c *nudgeEffectStartupCapabilities) { c.AtomicCommandRepository = false },
		},
		"trusted ingress": {
			wantReason: "trusted command ingress",
			remove:     func(c *nudgeEffectStartupCapabilities) { c.TrustedIngress = false },
		},
		"independent authority": {
			wantReason: "independently durable authority",
			remove:     func(c *nudgeEffectStartupCapabilities) { c.IndependentAuthority = false },
		},
		"trusted partition": {
			wantReason: "trusted city partition",
			remove:     func(c *nudgeEffectStartupCapabilities) { c.TrustedCityPartition = false },
		},
		"trusted partition resolver": {
			wantReason: "trusted city partition resolver",
			remove:     func(c *nudgeEffectStartupCapabilities) { c.TrustedCityPartitionResolver = false },
		},
		"claim authorizer": {
			wantReason: "claim authorizer",
			remove:     func(c *nudgeEffectStartupCapabilities) { c.ClaimAuthorizer = false },
		},
		"provider effect boundary": {
			wantReason: "runtime nudge effect provider",
			remove:     func(c *nudgeEffectStartupCapabilities) { c.ProviderEffect = false },
		},
	}
	for name, test := range tests {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			capabilities := completeNudgeEffectStartupCapabilities()
			test.remove(&capabilities)

			auto, err := selectNudgeEffectOwnership(
				context.Background(),
				rollout.ForTest(rollout.WithNudgeEffectOwner(rollout.Auto)),
				string(nudgequeue.CommandSecurityProfileStoreWriterIsController),
				func(context.Context) nudgeEffectStartupCapabilities { return capabilities },
			)
			if err != nil {
				t.Fatalf("auto selection: %v", err)
			}
			if auto.Ownership != nudgeEffectOwnershipLegacy {
				t.Fatalf("auto ownership = %d, want retained legacy", auto.Ownership)
			}
			if notice := strings.ToLower(auto.Notice); !strings.Contains(notice, "retaining legacy") || !strings.Contains(notice, test.wantReason) {
				t.Fatalf("auto notice = %q, want retained-legacy reason %q", auto.Notice, test.wantReason)
			}

			required, err := selectNudgeEffectOwnership(
				context.Background(),
				rollout.ForTest(rollout.WithNudgeEffectOwner(rollout.Require)),
				string(nudgequeue.CommandSecurityProfileStoreWriterIsController),
				func(context.Context) nudgeEffectStartupCapabilities { return capabilities },
			)
			if required.Ownership != nudgeEffectOwnershipLegacy {
				t.Fatalf("require refusal ownership = %d, want zero/legacy value", required.Ownership)
			}
			assertNudgeEffectStartupRefusal(t, err, rollout.Require, nudgequeue.CommandSecurityProfileStoreWriterIsController, test.wantReason)
		})
	}
}

func TestSelectNudgeEffectOwnershipInvalidOrHostedProfileAlwaysRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		mode         rollout.Mode
		profile      nudgequeue.CommandSecurityProfile
		capabilities nudgeEffectStartupCapabilities
		wantReason   string
	}{
		{
			name:         "omitted auto",
			mode:         rollout.Auto,
			profile:      "",
			capabilities: completeNudgeEffectStartupCapabilities(),
			wantReason:   "explicit supported",
		},
		{
			name:         "unknown require",
			mode:         rollout.Require,
			profile:      "store_writer",
			capabilities: completeNudgeEffectStartupCapabilities(),
			wantReason:   "explicit supported",
		},
		{
			name:    "hosted passes actual incomplete capabilities to security check",
			mode:    rollout.Auto,
			profile: nudgequeue.CommandSecurityProfileHosted,
			capabilities: func() nudgeEffectStartupCapabilities {
				c := completeNudgeEffectStartupCapabilities()
				c.CommandSecurity.ClaimAuthorizationAvailable = false
				return c
			}(),
			wantReason: "hosted claim authorization is unavailable",
		},
		{
			name:         "hosted remains unsupported even with complete profile capabilities",
			mode:         rollout.Require,
			profile:      nudgequeue.CommandSecurityProfileHosted,
			capabilities: completeNudgeEffectStartupCapabilities(),
			wantReason:   "hosted keyed ownership is unavailable",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			selection, err := selectNudgeEffectOwnership(
				context.Background(),
				rollout.ForTest(rollout.WithNudgeEffectOwner(test.mode)),
				string(test.profile),
				func(context.Context) nudgeEffectStartupCapabilities { return test.capabilities },
			)
			if selection.Ownership != nudgeEffectOwnershipLegacy || selection.Notice != "" {
				t.Fatalf("refused selection = %#v, want inert zero value", selection)
			}
			assertNudgeEffectStartupRefusal(t, err, test.mode, test.profile, test.wantReason)
		})
	}
}

func TestCurrentProductionNudgeEffectStartupCapabilitiesFailClosed(t *testing.T) {
	t.Parallel()
	cfg := &config.City{Daemon: config.DaemonConfig{NudgeDispatcher: "supervisor"}}
	capabilities := currentProductionNudgeEffectStartupCapabilities(cfg, runtime.NewFake())
	if !capabilities.SupervisorDispatcher {
		t.Fatal("production supervisor dispatcher capability = false")
	}
	if !capabilities.ProviderEffect {
		t.Fatal("production fake provider capability = false, want NudgeEffectProvider detected")
	}
	if capabilities.AtomicCommandRepository || capabilities.TrustedIngress || capabilities.IndependentAuthority ||
		capabilities.TrustedCityPartition || capabilities.TrustedCityPartitionResolver || capabilities.ClaimAuthorizer {
		t.Fatalf("production authority capabilities unexpectedly complete: %#v", capabilities)
	}
	security := capabilities.CommandSecurity
	if security.ProtectedNamespace || !security.WorkCredentialsCanWriteCommands || security.TrustedIngressAvailable || security.ClaimAuthorizationAvailable {
		t.Fatalf("production command security capabilities = %#v, want current shared-store fail-closed evidence", security)
	}

	withoutEffect := currentProductionNudgeEffectStartupCapabilities(cfg, &providerWithoutNudgeEffect{Provider: runtime.NewFake()})
	if withoutEffect.ProviderEffect {
		t.Fatal("provider without NudgeEffectProvider extension reported effect capable")
	}
	legacyDispatcher := currentProductionNudgeEffectStartupCapabilities(&config.City{}, runtime.NewFake())
	if legacyDispatcher.SupervisorDispatcher {
		t.Fatal("legacy dispatcher reported supervisor capability")
	}
}

func TestResolveNudgeEffectStartupUsesOneBootLatchAndCurrentEvidence(t *testing.T) {
	t.Parallel()
	localProfile := string(nudgequeue.CommandSecurityProfileStoreWriterIsController)

	autoCfg := &config.City{
		Beads: config.BeadsConfig{CommandSecurityProfile: localProfile},
		Daemon: config.DaemonConfig{
			NudgeDispatcher:  "supervisor",
			NudgeEffectOwner: "auto",
		},
	}
	flags, selection, err := resolveNudgeEffectStartup(context.Background(), autoCfg, runtime.NewFake())
	if err != nil {
		t.Fatalf("resolveNudgeEffectStartup auto: %v", err)
	}
	if flags.NudgeEffectOwner() != rollout.Auto {
		t.Fatalf("resolved mode = %q, want auto", flags.NudgeEffectOwner())
	}
	if selection.Ownership != nudgeEffectOwnershipLegacy || !strings.Contains(strings.ToLower(selection.Notice), "retaining legacy") {
		t.Fatalf("auto selection = %#v, want loud retained legacy", selection)
	}

	requireCfg := *autoCfg
	requireCfg.Daemon.NudgeEffectOwner = "require"
	flags, selection, err = resolveNudgeEffectStartup(context.Background(), &requireCfg, runtime.NewFake())
	if flags.NudgeEffectOwner() != rollout.Require {
		t.Fatalf("resolved mode = %q, want require", flags.NudgeEffectOwner())
	}
	if selection != (nudgeEffectStartupSelection{}) {
		t.Fatalf("require selection = %#v, want inert zero value", selection)
	}
	assertNudgeEffectStartupRefusal(t, err, rollout.Require, nudgequeue.CommandSecurityProfileStoreWriterIsController, "atomic command repository")

	offCfg := &config.City{
		Beads:  config.BeadsConfig{CommandSecurityProfile: "invalid-but-uninspected"},
		Daemon: config.DaemonConfig{NudgeEffectOwner: "off"},
	}
	flags, selection, err = resolveNudgeEffectStartup(context.Background(), offCfg, nil)
	if err != nil {
		t.Fatalf("resolveNudgeEffectStartup off: %v", err)
	}
	if flags.NudgeEffectOwner() != rollout.Off || selection.Ownership != nudgeEffectOwnershipLegacy || selection.Notice != "" {
		t.Fatalf("off result = flags %q selection %#v, want silent legacy", flags.NudgeEffectOwner(), selection)
	}
}

func completeNudgeEffectStartupCapabilities() nudgeEffectStartupCapabilities {
	return nudgeEffectStartupCapabilities{
		SupervisorDispatcher:         true,
		AtomicCommandRepository:      true,
		TrustedIngress:               true,
		IndependentAuthority:         true,
		TrustedCityPartition:         true,
		TrustedCityPartitionResolver: true,
		ClaimAuthorizer:              true,
		ProviderEffect:               true,
		CommandSecurity: nudgequeue.CommandSecurityCapabilities{
			ProtectedNamespace:              true,
			WorkCredentialsCanWriteCommands: false,
			TrustedIngressAvailable:         true,
			ClaimAuthorizationAvailable:     true,
		},
	}
}

func assertNudgeEffectStartupRefusal(
	t *testing.T,
	err error,
	wantMode rollout.Mode,
	wantProfile nudgequeue.CommandSecurityProfile,
	wantReason string,
) {
	t.Helper()
	if !errors.Is(err, errNudgeEffectStartupRefused) {
		t.Fatalf("error = %v, want errNudgeEffectStartupRefused", err)
	}
	var refusal *nudgeEffectStartupRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("error type = %T, want *nudgeEffectStartupRefusal", err)
	}
	if refusal.Mode != wantMode || refusal.Profile != wantProfile {
		t.Fatalf("refusal = %#v, want mode=%q profile=%q", refusal, wantMode, wantProfile)
	}
	if !strings.Contains(strings.ToLower(refusal.Reason), strings.ToLower(wantReason)) {
		t.Fatalf("refusal reason = %q, want to contain %q", refusal.Reason, wantReason)
	}
}

type providerWithoutNudgeEffect struct {
	runtime.Provider
}
