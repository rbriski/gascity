package nudgequeue

import (
	"fmt"
	"testing"
)

func TestLocalAuthorityDensePrefixAdvancesOneBoundedDurablePage(t *testing.T) {
	state := localAuthorityRepositoryState()
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	tx, err := authority.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin decision seed: %v", err)
	}
	for sequence := uint64(1); sequence <= 600; sequence++ {
		digest := make([]byte, 32)
		digest[0] = byte(sequence)
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO admission_decisions
			(sequence, command_id, decision_kind, allocation_revision, decision_revision,
			 origin_digest, identity_digest, terminal_revision, terminal_digest, rejection_reason)
			 VALUES (?, ?, 'rejected', ?, ?, ?, ?, ?, ?, 'unauthorized_provenance')`,
			encodeLocalAuthorityUint64(sequence), fmt.Sprintf("command-%04d", sequence),
			encodeLocalAuthorityUint64(sequence), encodeLocalAuthorityUint64(sequence),
			digest, digest, encodeLocalAuthorityUint64(sequence), digest); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert decision %d: %v", sequence, err)
		}
	}
	if err := advanceLocalAuthorityDensePrefix(t.Context(), tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("advanceLocalAuthorityDensePrefix: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit bounded prefix: %v", err)
	}

	dense, err := authority.localAuthorityDenseDecisionHighWater(t.Context())
	if err != nil {
		t.Fatalf("localAuthorityDenseDecisionHighWater: %v", err)
	}
	if dense != localAuthorityRecoveryPageSize {
		t.Fatalf("one dense-prefix transaction advanced to %d, want bounded page %d", dense, localAuthorityRecoveryPageSize)
	}
	advanced, err := authority.advanceDenseDecisionPrefixPage(t.Context(), localAuthorityRecoveryPageSize)
	if err != nil || advanced != localAuthorityRecoveryPageSize {
		t.Fatalf("second dense-prefix page advanced %d, err=%v; want %d", advanced, err, localAuthorityRecoveryPageSize)
	}
	dense, err = authority.localAuthorityDenseDecisionHighWater(t.Context())
	if err != nil || dense != 2*localAuthorityRecoveryPageSize {
		t.Fatalf("durable dense prefix after second page = %d, err=%v; want %d", dense, err, 2*localAuthorityRecoveryPageSize)
	}
	advanced, err = authority.advanceDenseDecisionPrefixPage(t.Context(), localAuthorityRecoveryPageSize)
	if err != nil || advanced != 600-2*localAuthorityRecoveryPageSize {
		t.Fatalf("final dense-prefix page advanced %d, err=%v; want %d", advanced, err, 600-2*localAuthorityRecoveryPageSize)
	}
	dense, err = authority.localAuthorityDenseDecisionHighWater(t.Context())
	if err != nil || dense != 600 {
		t.Fatalf("resumed dense prefix = %d, err=%v; want 600", dense, err)
	}
}
