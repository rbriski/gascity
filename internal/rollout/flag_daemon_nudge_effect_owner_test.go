package rollout

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestResolveDaemonNudgeEffectOwner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        string
		want       Mode
		wantOrigin Origin
		wantErr    bool
	}{
		{name: "omitted", want: Off, wantOrigin: OriginBuiltin},
		{name: "off", raw: "off", want: Off, wantOrigin: OriginConfig},
		{name: "auto", raw: "auto", want: Auto, wantOrigin: OriginConfig},
		{name: "require", raw: "require", want: Require, wantOrigin: OriginConfig},
		{name: "invalid", raw: "enabled", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			flags, err := Resolve(&config.City{Daemon: config.DaemonConfig{NudgeEffectOwner: test.raw}}, ResolveOptions{
				LookupEnv: func(string) (string, bool) { return "", false },
			})
			if (err != nil) != test.wantErr {
				t.Fatalf("Resolve error = %v, wantErr=%v", err, test.wantErr)
			}
			if test.wantErr {
				return
			}
			if got := flags.NudgeEffectOwner(); got != test.want {
				t.Fatalf("NudgeEffectOwner = %q, want %q", got, test.want)
			}
			if got := flags.OriginOf(KeyDaemonNudgeEffectOwner); got != test.wantOrigin {
				t.Fatalf("OriginOf = %q, want %q", got, test.wantOrigin)
			}
			if got := flags.ValueOf(KeyDaemonNudgeEffectOwner); got != string(test.want) {
				t.Fatalf("ValueOf = %q, want %q", got, test.want)
			}
		})
	}
}

func TestForTestNudgeEffectOwnerIsInstanceLocal(t *testing.T) {
	t.Parallel()
	if got := ForTest().NudgeEffectOwner(); got != Off {
		t.Fatalf("ForTest default = %q, want off", got)
	}
	require := ForTest(WithNudgeEffectOwner(Require))
	off := ForTest(WithNudgeEffectOwner(Off))
	if require.NudgeEffectOwner() != Require || off.NudgeEffectOwner() != Off {
		t.Fatalf("ForTest modes = %q/%q, want require/off", require.NudgeEffectOwner(), off.NudgeEffectOwner())
	}
}

func TestZeroFlagsKeepsNudgeEffectOwnerLegacy(t *testing.T) {
	t.Parallel()
	var flags Flags
	if got := flags.NudgeEffectOwner(); got != ModeUnset {
		t.Fatalf("zero Flags nudge owner = %q, want ModeUnset", got)
	}
	if got := flags.OriginOf(KeyDaemonNudgeEffectOwner); got != "" {
		t.Fatalf("zero Flags origin = %q, want empty", got)
	}
}

func TestNudgeEffectOwnerRegistryBinding(t *testing.T) {
	t.Parallel()
	var found bool
	for _, spec := range Specs() {
		if spec.Key != KeyDaemonNudgeEffectOwner {
			continue
		}
		found = true
		if spec.ConfigPath != "daemon.nudge_effect_owner" {
			t.Fatalf("ConfigPath = %q", spec.ConfigPath)
		}
		if spec.Default.Mode == nil || *spec.Default.Mode != Off {
			t.Fatalf("Default = %#v, want mode off", spec.Default)
		}
		if spec.EnvOverride != "" {
			t.Fatalf("EnvOverride = %q, want none for boot-only owner handoff", spec.EnvOverride)
		}
	}
	if !found {
		t.Fatalf("registry missing %s", KeyDaemonNudgeEffectOwner)
	}
}
