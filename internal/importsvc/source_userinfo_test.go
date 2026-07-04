package importsvc

import (
	"strings"
	"testing"
)

func TestRejectSourceUserinfo(t *testing.T) {
	tests := []struct {
		name       string
		source     string
		wantReject bool
	}{
		{"https with user:token", "https://user:ghp_secret@github.com/org/repo", true},
		{"https with user only", "https://user@github.com/org/repo", true},
		{"file with user:pass", "file://user:pass@/repo", true},
		{"clean https", "https://github.com/org/repo", false},
		{"scp form", "git@github.com:org/repo.git", false},
		{"ssh url with user", "ssh://git@github.com/org/repo", false},
		{"shorthand", "github.com/org/repo", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := rejectSourceUserinfo(tc.source)
			if tc.wantReject && err == nil {
				t.Fatalf("expected rejection for %q", tc.source)
			}
			if !tc.wantReject && err != nil {
				t.Fatalf("unexpected rejection for %q: %v", tc.source, err)
			}
			if err != nil {
				if strings.Contains(err.Error(), "ghp_secret") || strings.Contains(err.Error(), ":pass@") {
					t.Fatalf("error leaked the secret: %v", err)
				}
				if !strings.Contains(err.Error(), "***@") {
					t.Fatalf("error should carry a redacted source, got %v", err)
				}
			}
		})
	}
}
