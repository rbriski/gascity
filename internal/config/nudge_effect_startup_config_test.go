package config

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestNudgeEffectStartupConfigIsExplicitAndStrict(t *testing.T) {
	t.Run("omitted values stay inert", func(t *testing.T) {
		cfg, err := Parse(nil)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if cfg.Beads.CommandSecurityProfile != "" {
			t.Fatalf("omitted command_security_profile = %q, want empty", cfg.Beads.CommandSecurityProfile)
		}
		if got := cfg.Daemon.NudgeEffectOwnerMode(); got != "off" {
			t.Fatalf("omitted nudge_effect_owner = %q, want off", got)
		}
	})

	t.Run("explicit local require decodes", func(t *testing.T) {
		cfg, err := Parse([]byte(`
[beads]
command_security_profile = "store_writer_is_controller"

[daemon]
nudge_effect_owner = "require"
`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if cfg.Beads.CommandSecurityProfile != "store_writer_is_controller" {
			t.Fatalf("command_security_profile = %q", cfg.Beads.CommandSecurityProfile)
		}
		if got := cfg.Daemon.NudgeEffectOwnerMode(); got != "require" {
			t.Fatalf("nudge_effect_owner = %q, want require", got)
		}
	})

	for _, test := range []struct {
		name string
		toml string
		want string
	}{
		{
			name: "unknown security profile",
			toml: "[beads]\ncommand_security_profile = \"store_writer\"\n",
			want: "beads.command_security_profile",
		},
		{
			name: "unknown ownership mode",
			toml: "[daemon]\nnudge_effect_owner = \"enabled\"\n",
			want: "daemon.nudge_effect_owner",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(test.toml))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse error = %v, want strict %s refusal", err, test.want)
			}
		})
	}
}

func TestNudgeEffectStartupConfigSurvivesUnrelatedFragments(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test-city"

[beads]
command_security_profile = "store_writer_is_controller"

[daemon]
nudge_effect_owner = "require"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
[beads]
bd_compatibility = "bd-1.0.5"

[daemon]
patrol_interval = "1m"
`)

	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := cfg.Beads.CommandSecurityProfile; got != "store_writer_is_controller" {
		t.Fatalf("command_security_profile = %q, want root value to survive [beads] fragment", got)
	}
	if got := cfg.Daemon.NudgeEffectOwnerMode(); got != "require" {
		t.Fatalf("nudge_effect_owner = %q, want root value to survive [daemon] fragment", got)
	}
}

func TestNudgeEffectStartupConfigFragmentOverridesExplicitly(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test-city"

[beads]
command_security_profile = "store_writer_is_controller"

[daemon]
nudge_effect_owner = "require"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
[beads]
command_security_profile = "hosted"

[daemon]
nudge_effect_owner = "auto"
`)

	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := cfg.Beads.CommandSecurityProfile; got != "hosted" {
		t.Fatalf("command_security_profile = %q, want fragment hosted", got)
	}
	if got := cfg.Daemon.NudgeEffectOwnerMode(); got != "auto" {
		t.Fatalf("nudge_effect_owner = %q, want fragment auto", got)
	}
}
