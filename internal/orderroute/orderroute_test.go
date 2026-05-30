package orderroute

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

func TestQualifyPoolContract(t *testing.T) {
	tests := []struct {
		name          string
		cfg           *config.City
		pool          string
		rig           string
		sourceDirHint string
		want          string
		wantErr       string
	}{
		{
			name: "nil config preserves city pool",
			cfg:  nil,
			pool: "runner",
			want: "runner",
		},
		{
			name: "nil config preserves rig-qualified fallback",
			cfg:  nil,
			pool: "runner",
			rig:  "api",
			want: "api/runner",
		},
		{
			name: "nil config preserves slash-qualified pool",
			cfg:  nil,
			pool: "api/runner",
			rig:  "api",
			want: "api/runner",
		},
		{
			name: "slash-qualified passes through",
			cfg: &config.City{Agents: []config.Agent{
				{Name: "runner", BindingName: "shared"},
			}},
			pool: "api/runner",
			rig:  "other",
			want: "api/runner",
		},
		{
			name: "binding-qualified rig pool gets rig prefix",
			cfg: &config.City{Agents: []config.Agent{
				{Name: "runner", BindingName: "api-pack", Dir: "api"},
			}},
			pool: "api-pack.runner",
			rig:  "api",
			want: "api/api-pack.runner",
		},
		{
			name: "city order resolves binding",
			cfg: &config.City{Agents: []config.Agent{
				{Name: "runner", BindingName: "shared"},
			}},
			pool: "runner",
			want: "shared.runner",
		},
		{
			name: "city order preserves no-binding agent",
			cfg: &config.City{Agents: []config.Agent{
				{Name: "runner"},
			}},
			pool: "runner",
			want: "runner",
		},
		{
			name: "city order miss falls through",
			cfg: &config.City{Agents: []config.Agent{
				{Name: "runner", BindingName: "shared"},
			}},
			pool: "builder",
			want: "builder",
		},
		{
			name: "city local shadow wins without source hint",
			cfg: &config.City{Agents: []config.Agent{
				{Name: "runner"},
				{Name: "runner", BindingName: "shared", SourceDir: "/city/packs/shared"},
				{Name: "runner", BindingName: "tools", SourceDir: "/city/packs/tools"},
			}},
			pool: "runner",
			want: "runner",
		},
		{
			name: "city source hint beats local shadow",
			cfg: &config.City{Agents: []config.Agent{
				{Name: "runner"},
				{Name: "runner", BindingName: "shared", SourceDir: "/city/packs/shared"},
				{Name: "runner", BindingName: "tools", SourceDir: "/city/packs/tools"},
			}},
			pool:          "runner",
			sourceDirHint: "/city/packs/tools",
			want:          "tools.runner",
		},
		{
			name: "city imported collision without hint is ambiguous",
			cfg: &config.City{Agents: []config.Agent{
				{Name: "runner", BindingName: "shared", SourceDir: "/city/packs/shared"},
				{Name: "runner", BindingName: "tools", SourceDir: "/city/packs/tools"},
			}},
			pool:    "runner",
			wantErr: `ambiguous pool "runner" for city order: matches shared.runner, tools.runner`,
		},
		{
			name: "city source hint disambiguates imported collision",
			cfg: &config.City{Agents: []config.Agent{
				{Name: "runner", BindingName: "shared", SourceDir: "/city/packs/shared"},
				{Name: "runner", BindingName: "tools", SourceDir: "/city/packs/tools"},
			}},
			pool:          "runner",
			sourceDirHint: "/city/packs/shared",
			want:          "shared.runner",
		},
		{
			name: "rig-local match wins",
			cfg: &config.City{Agents: []config.Agent{
				{Name: "runner", BindingName: "shared"},
				{Name: "runner", BindingName: "api-pack", Dir: "api"},
			}},
			pool: "runner",
			rig:  "api",
			want: "api/api-pack.runner",
		},
		{
			name: "rig order falls back to city pool",
			cfg: &config.City{Agents: []config.Agent{
				{Name: "runner", BindingName: "shared"},
			}},
			pool: "runner",
			rig:  "api",
			want: "shared.runner",
		},
		{
			name: "binding-qualified rig order falls back to city pool",
			cfg: &config.City{Agents: []config.Agent{
				{Name: "runner", BindingName: "shared"},
			}},
			pool: "shared.runner",
			rig:  "api",
			want: "shared.runner",
		},
		{
			name: "rig local pool shadows city fallback",
			cfg: &config.City{Agents: []config.Agent{
				{Name: "runner", BindingName: "shared"},
				{Name: "runner", BindingName: "api-pack", Dir: "api"},
			}},
			pool: "runner",
			rig:  "api",
			want: "api/api-pack.runner",
		},
		{
			name: "rig order ambiguous city fallback returns error",
			cfg: &config.City{Agents: []config.Agent{
				{Name: "runner", BindingName: "shared"},
				{Name: "runner", BindingName: "tools"},
			}},
			pool:    "runner",
			rig:     "api",
			wantErr: `ambiguous pool "runner" for city order: matches shared.runner, tools.runner`,
		},
		{
			name: "source hint disambiguates city imports",
			cfg: &config.City{Agents: []config.Agent{
				{Name: "runner", BindingName: "alpha", SourceDir: "/city/packs/alpha"},
				{Name: "runner", BindingName: "beta", SourceDir: "/city/packs/beta"},
			}},
			pool:          "runner",
			sourceDirHint: "/city/packs/beta",
			want:          "beta.runner",
		},
		{
			name: "ambiguous city pool returns error",
			cfg: &config.City{Agents: []config.Agent{
				{Name: "runner", BindingName: "alpha"},
				{Name: "runner", BindingName: "beta"},
			}},
			pool:    "runner",
			wantErr: `ambiguous pool "runner" for city order: matches alpha.runner, beta.runner`,
		},
		{
			name: "unresolved dotted city pool passes through",
			cfg: &config.City{Agents: []config.Agent{
				{Name: "runner", BindingName: "shared"},
			}},
			pool: "team.runner",
			want: "team.runner",
		},
		{
			name: "empty config agents falls through",
			cfg:  &config.City{},
			pool: "runner",
			want: "runner",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := QualifyPool(tt.pool, tt.rig, tt.cfg, tt.sourceDirHint)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("QualifyPool() error = nil, want %q", tt.wantErr)
				}
				if err.Error() != tt.wantErr {
					t.Fatalf("QualifyPool() error = %q, want %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("QualifyPool() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("QualifyPool() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQualifyOrderPoolUsesFormulaLayerSourceHint(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{
		{Name: "runner", BindingName: "alpha", SourceDir: "/city/packs/alpha"},
		{Name: "runner", BindingName: "beta", SourceDir: "/city/packs/beta"},
	}}
	order := orders.Order{
		Name:         "nightly",
		Pool:         "runner",
		FormulaLayer: "/city/packs/alpha/formulas",
	}

	got, err := QualifyOrderPool(order, cfg)
	if err != nil {
		t.Fatalf("QualifyOrderPool() error = %v", err)
	}
	if got != "alpha.runner" {
		t.Fatalf("QualifyOrderPool() = %q, want alpha.runner", got)
	}
}
