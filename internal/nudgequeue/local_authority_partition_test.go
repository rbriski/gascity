package nudgequeue

import (
	"errors"
	"os"
	"runtime"
	"testing"
	"time"
)

func TestLocalNudgeAuthorityTrustedCityPartitionMatchesAdmissionAndRestart(t *testing.T) {
	cityPath := t.TempDir()
	repository := newVerifiedCommandRepository(t, newRepositoryAtomicTestStore())
	state, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("repository State: %v", err)
	}
	authority, err := OpenLocalNudgeAuthority(t.Context(), cityPath, state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	startupPartition, err := authority.TrustedCityPartition(t.Context())
	if err != nil {
		t.Fatalf("TrustedCityPartition before any command: %v", err)
	}
	if !startupPartition.valid() {
		t.Fatal("TrustedCityPartition before any command is invalid")
	}

	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	admitted, err := ingress.Admit(
		WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester()),
		validNudgeIngressRequest(now),
	)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if admitted.Partition != startupPartition {
		t.Fatalf("admitted partition = %#v, want startup partition %#v", admitted.Partition, startupPartition)
	}

	state, err = repository.State(t.Context())
	if err != nil {
		t.Fatalf("repository State after admission: %v", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatalf("Close first authority: %v", err)
	}
	reopened, err := OpenLocalNudgeAuthority(t.Context(), cityPath, state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("reopen LocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restartedPartition, err := reopened.TrustedCityPartition(t.Context())
	if err != nil {
		t.Fatalf("TrustedCityPartition after restart: %v", err)
	}
	if restartedPartition != startupPartition {
		t.Fatalf("restarted partition = %#v, want %#v", restartedPartition, startupPartition)
	}
}

func TestLocalNudgeAuthorityTrustedCityPartitionRequiresOpenJournal(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		var authority *LocalNudgeAuthority
		partition, err := authority.TrustedCityPartition(t.Context())
		if partition != (TrustedCityPartition{}) || !errors.Is(err, ErrLocalNudgeAuthorityUnavailable) {
			t.Fatalf("TrustedCityPartition on nil authority = %#v, err=%v; want zero and unavailable", partition, err)
		}
	})

	t.Run("config only", func(t *testing.T) {
		authority := &LocalNudgeAuthority{opts: localAuthorityOptions()}
		partition, err := authority.TrustedCityPartition(t.Context())
		if partition != (TrustedCityPartition{}) || !errors.Is(err, ErrLocalNudgeAuthorityUnavailable) {
			t.Fatalf("TrustedCityPartition on config-only authority = %#v, err=%v; want zero and unavailable", partition, err)
		}
	})

	t.Run("closed", func(t *testing.T) {
		authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), localAuthorityRepositoryState(), localAuthorityOptions())
		if err != nil {
			t.Fatalf("OpenLocalNudgeAuthority: %v", err)
		}
		if err := authority.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		partition, err := authority.TrustedCityPartition(t.Context())
		if partition != (TrustedCityPartition{}) || !errors.Is(err, ErrLocalNudgeAuthorityUnavailable) {
			t.Fatalf("TrustedCityPartition after Close = %#v, err=%v; want zero and unavailable", partition, err)
		}
	})
}

func TestLocalNudgeAuthorityTrustedCityPartitionRejectsReplacedJournal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("open-file replacement semantics are Unix-specific")
	}
	cityPath := t.TempDir()
	authority, err := OpenLocalNudgeAuthority(t.Context(), cityPath, localAuthorityRepositoryState(), localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	path := LocalNudgeAuthorityPath(cityPath)
	if err := os.Rename(path, path+".replaced"); err != nil {
		t.Fatalf("rename authority journal: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write replacement authority journal: %v", err)
	}

	partition, err := authority.TrustedCityPartition(t.Context())
	if partition != (TrustedCityPartition{}) || !errors.Is(err, ErrLocalNudgeAuthorityUnavailable) {
		t.Fatalf("TrustedCityPartition after replacement = %#v, err=%v; want zero and unavailable", partition, err)
	}
}

func TestLocalNudgeAuthorityTrustedCityPartitionRejectsTamperedJournal(t *testing.T) {
	for _, test := range []struct {
		name   string
		tamper string
	}{
		{name: "issuer", tamper: `UPDATE authority_meta SET issuer = 'tampered-issuer' WHERE singleton = 1`},
		{name: "tenant scope", tamper: `UPDATE authority_meta SET tenant_scope = 'tampered-tenant' WHERE singleton = 1`},
		{name: "city scope", tamper: `UPDATE authority_meta SET city_scope = 'tampered-city' WHERE singleton = 1`},
		{name: "schema", tamper: `CREATE TABLE injected_authority_state (value TEXT)`},
	} {
		t.Run(test.name, func(t *testing.T) {
			authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), localAuthorityRepositoryState(), localAuthorityOptions())
			if err != nil {
				t.Fatalf("OpenLocalNudgeAuthority: %v", err)
			}
			t.Cleanup(func() { _ = authority.Close() })
			if _, err := authority.db.ExecContext(t.Context(), test.tamper); err != nil {
				t.Fatalf("tamper authority journal: %v", err)
			}

			partition, err := authority.TrustedCityPartition(t.Context())
			if partition != (TrustedCityPartition{}) || !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
				t.Fatalf("TrustedCityPartition after %s tamper = %#v, err=%v; want zero and conflict", test.name, partition, err)
			}
		})
	}
}
