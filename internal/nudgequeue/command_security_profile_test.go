package nudgequeue

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckCommandSecurityProfileHostedRequiresEveryControlBoundary(t *testing.T) {
	complete := CommandSecurityCapabilities{
		ProtectedNamespace:              true,
		WorkCredentialsCanWriteCommands: false,
		TrustedIngressAvailable:         true,
		ClaimAuthorizationAvailable:     true,
	}
	for name, mutate := range map[string]func(*CommandSecurityCapabilities){
		"protected namespace missing": func(c *CommandSecurityCapabilities) { c.ProtectedNamespace = false },
		"work credential can write commands": func(c *CommandSecurityCapabilities) {
			c.WorkCredentialsCanWriteCommands = true
		},
		"trusted ingress missing":     func(c *CommandSecurityCapabilities) { c.TrustedIngressAvailable = false },
		"claim authorization missing": func(c *CommandSecurityCapabilities) { c.ClaimAuthorizationAvailable = false },
	} {
		t.Run(name, func(t *testing.T) {
			capabilities := complete
			mutate(&capabilities)
			if _, err := CheckCommandSecurityProfile(CommandSecurityProfileHosted, capabilities); !errors.Is(err, ErrCommandSecurityProfile) {
				t.Fatalf("CheckCommandSecurityProfile error = %v, want hosted refusal", err)
			}
		})
	}

	status, err := CheckCommandSecurityProfile(CommandSecurityProfileHosted, complete)
	if err != nil {
		t.Fatalf("CheckCommandSecurityProfile complete hosted profile: %v", err)
	}
	if status.Warning != "" {
		t.Fatalf("hosted warning = %q, want empty", status.Warning)
	}
}

func TestCheckCommandSecurityProfileLocalStoreWriterAuthorityIsExplicitAndWarns(t *testing.T) {
	status, err := CheckCommandSecurityProfile(CommandSecurityProfileStoreWriterIsController, CommandSecurityCapabilities{})
	if err != nil {
		t.Fatalf("CheckCommandSecurityProfile local profile: %v", err)
	}
	warning := strings.ToLower(status.Warning)
	for _, required := range []string{"store credential", "full session-control authority", "local single-tenant"} {
		if !strings.Contains(warning, required) {
			t.Fatalf("local warning %q does not contain %q", status.Warning, required)
		}
	}

	for _, profile := range []CommandSecurityProfile{"", "store_writer", "controller", "unknown"} {
		if _, err := CheckCommandSecurityProfile(profile, CommandSecurityCapabilities{}); !errors.Is(err, ErrCommandSecurityProfile) {
			t.Fatalf("profile %q error = %v, want explicit-profile refusal", profile, err)
		}
	}
}
