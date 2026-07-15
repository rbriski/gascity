package nudgequeue

import (
	"errors"
	"math"
	"testing"
)

func TestDecideRestoreAnchorClassifiesLineageTransitions(t *testing.T) {
	anchor := RestoreAnchor{
		Version:                 RestoreAnchorVersion1,
		Store:                   CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: 7},
		HighestAcceptedRevision: 41,
		HighestAcceptedSequence: 17,
	}
	tests := []struct {
		name                  string
		check                 RestoreAnchorCheck
		want                  RestoreAnchorDisposition
		wantEffects           bool
		wantRetainedRevision  uint64
		wantRetainedSequence  uint64
		wantRequiredEpoch     uint64
		wantCandidateRevision uint64
		wantCandidateSequence uint64
		wantCandidateEpoch    uint64
	}{
		{
			name: "first initialization remains frozen until explicitly persisted",
			check: RestoreAnchorCheck{
				DatabaseStore:    CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: 1},
				DatabaseRevision: 0,
			},
			want:              RestoreAnchorFirstInitialization,
			wantEffects:       false,
			wantRequiredEpoch: 1,
		},
		{
			name: "equal",
			check: RestoreAnchorCheck{
				Persisted:                 &anchor,
				DatabaseStore:             anchor.Store,
				DatabaseRevision:          anchor.HighestAcceptedRevision,
				DatabaseSequenceHighWater: anchor.HighestAcceptedSequence,
			},
			want:                 RestoreAnchorEqual,
			wantEffects:          true,
			wantRetainedRevision: 41,
			wantRetainedSequence: 17,
		},
		{
			name: "normal advance must persist before effects",
			check: RestoreAnchorCheck{
				Persisted:                 &anchor,
				DatabaseStore:             anchor.Store,
				DatabaseRevision:          42,
				DatabaseSequenceHighWater: 18,
			},
			want:                  RestoreAnchorAdvanceRequired,
			wantEffects:           false,
			wantRetainedRevision:  41,
			wantRetainedSequence:  17,
			wantCandidateRevision: 42,
			wantCandidateSequence: 18,
			wantCandidateEpoch:    7,
		},
		{
			name: "same epoch database rewind",
			check: RestoreAnchorCheck{
				Persisted:                 &anchor,
				DatabaseStore:             anchor.Store,
				DatabaseRevision:          40,
				DatabaseSequenceHighWater: 17,
			},
			want:                 RestoreAnchorDatabaseRewind,
			wantEffects:          false,
			wantRetainedRevision: 41,
			wantRetainedSequence: 17,
			wantRequiredEpoch:    8,
		},
		{
			name: "database epoch rewind",
			check: RestoreAnchorCheck{
				Persisted:                 &anchor,
				DatabaseStore:             CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: 6},
				DatabaseRevision:          99,
				DatabaseSequenceHighWater: 50,
			},
			want:                 RestoreAnchorDatabaseRewind,
			wantEffects:          false,
			wantRetainedRevision: 41,
			wantRetainedSequence: 17,
			wantRequiredEpoch:    8,
		},
		{
			name: "foreign store",
			check: RestoreAnchorCheck{
				Persisted:                 &anchor,
				DatabaseStore:             CommandStoreBinding{StoreUUID: "store-b", RestoreEpoch: 7},
				DatabaseRevision:          41,
				DatabaseSequenceHighWater: 17,
			},
			want:                 RestoreAnchorForeignStore,
			wantEffects:          false,
			wantRetainedRevision: 41,
			wantRetainedSequence: 17,
		},
		{
			name: "epoch advance requires explicit recovery",
			check: RestoreAnchorCheck{
				Persisted:                 &anchor,
				DatabaseStore:             CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: 9},
				DatabaseRevision:          3,
				DatabaseSequenceHighWater: 2,
			},
			want:                 RestoreAnchorEpochAdvance,
			wantEffects:          false,
			wantRetainedRevision: 41,
			wantRetainedSequence: 17,
			wantRequiredEpoch:    10,
		},
		{
			name: "sequence rewind despite newer revision",
			check: RestoreAnchorCheck{
				Persisted:                 &anchor,
				DatabaseStore:             anchor.Store,
				DatabaseRevision:          42,
				DatabaseSequenceHighWater: 16,
			},
			want:                 RestoreAnchorDatabaseRewind,
			wantEffects:          false,
			wantRetainedRevision: 41,
			wantRetainedSequence: 17,
			wantRequiredEpoch:    8,
		},
		{
			name: "anchor disappeared after acceptance",
			check: RestoreAnchorCheck{
				PreviouslyAccepted:        &anchor,
				DatabaseStore:             anchor.Store,
				DatabaseRevision:          41,
				DatabaseSequenceHighWater: 17,
			},
			want:                 RestoreAnchorInvalid,
			wantEffects:          false,
			wantRetainedRevision: 41,
			wantRetainedSequence: 17,
			wantRequiredEpoch:    8,
		},
		{
			name: "anchor revision rewound while running",
			check: RestoreAnchorCheck{
				Persisted: &RestoreAnchor{
					Version:                 RestoreAnchorVersion1,
					Store:                   anchor.Store,
					HighestAcceptedRevision: 40,
					HighestAcceptedSequence: 16,
				},
				PreviouslyAccepted:        &anchor,
				DatabaseStore:             anchor.Store,
				DatabaseRevision:          41,
				DatabaseSequenceHighWater: 17,
			},
			want:                 RestoreAnchorInvalid,
			wantEffects:          false,
			wantRetainedRevision: 41,
			wantRetainedSequence: 17,
			wantRequiredEpoch:    8,
		},
		{
			name: "anchor read failure retains the highest observed epoch",
			check: RestoreAnchorCheck{
				AnchorReadFailed:          true,
				PreviouslyAccepted:        &anchor,
				DatabaseStore:             CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: 100},
				DatabaseRevision:          41,
				DatabaseSequenceHighWater: 17,
			},
			want:                 RestoreAnchorInvalid,
			wantEffects:          false,
			wantRetainedRevision: 41,
			wantRetainedSequence: 17,
			wantRequiredEpoch:    101,
		},
		{
			name: "anchor read failure at maximum observed epoch has no representable recovery",
			check: RestoreAnchorCheck{
				AnchorReadFailed:          true,
				PreviouslyAccepted:        &anchor,
				DatabaseStore:             CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: math.MaxUint64},
				DatabaseRevision:          41,
				DatabaseSequenceHighWater: 17,
			},
			want:                 RestoreAnchorInvalid,
			wantEffects:          false,
			wantRetainedRevision: 41,
			wantRetainedSequence: 17,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideRestoreAnchor(tc.check)
			if got.Disposition != tc.want {
				t.Fatalf("Disposition = %q, want %q (decision: %#v)", got.Disposition, tc.want, got)
			}
			if got.EffectsAllowed != tc.wantEffects {
				t.Errorf("EffectsAllowed = %t, want %t", got.EffectsAllowed, tc.wantEffects)
			}
			if got.RetainedHighestRevision != tc.wantRetainedRevision {
				t.Errorf("RetainedHighestRevision = %d, want %d", got.RetainedHighestRevision, tc.wantRetainedRevision)
			}
			if got.RetainedHighestSequence != tc.wantRetainedSequence {
				t.Errorf("RetainedHighestSequence = %d, want %d", got.RetainedHighestSequence, tc.wantRetainedSequence)
			}
			if got.MinimumRecoveryEpoch != tc.wantRequiredEpoch {
				t.Errorf("MinimumRecoveryEpoch = %d, want %d", got.MinimumRecoveryEpoch, tc.wantRequiredEpoch)
			}
			if got.Candidate != nil && got.Candidate.HighestAcceptedRevision != tc.wantCandidateRevision {
				t.Errorf("Candidate.HighestAcceptedRevision = %d, want %d", got.Candidate.HighestAcceptedRevision, tc.wantCandidateRevision)
			}
			if got.Candidate != nil && got.Candidate.HighestAcceptedSequence != tc.wantCandidateSequence {
				t.Errorf("Candidate.HighestAcceptedSequence = %d, want %d", got.Candidate.HighestAcceptedSequence, tc.wantCandidateSequence)
			}
			if got.Candidate != nil && tc.wantCandidateEpoch != 0 && got.Candidate.Store.RestoreEpoch != tc.wantCandidateEpoch {
				t.Errorf("Candidate.RestoreEpoch = %d, want %d", got.Candidate.Store.RestoreEpoch, tc.wantCandidateEpoch)
			}
			if tc.wantCandidateRevision != 0 && got.Candidate == nil {
				t.Fatalf("Candidate = nil, want revision %d", tc.wantCandidateRevision)
			}
		})
	}
}

func TestDecideRestoreAnchorFailsClosedOnInvalidEvidence(t *testing.T) {
	validStore := CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: 1}
	tests := []RestoreAnchorCheck{
		{
			Persisted:        &RestoreAnchor{Version: 99, Store: validStore},
			DatabaseStore:    validStore,
			DatabaseRevision: 1,
		},
		{
			Persisted:        &RestoreAnchor{Version: RestoreAnchorVersion1, Store: CommandStoreBinding{RestoreEpoch: 1}},
			DatabaseStore:    validStore,
			DatabaseRevision: 1,
		},
		{
			Persisted:        &RestoreAnchor{Version: RestoreAnchorVersion1, Store: validStore},
			DatabaseStore:    CommandStoreBinding{StoreUUID: "store-a"},
			DatabaseRevision: 1,
		},
		{
			Persisted:                 &RestoreAnchor{Version: RestoreAnchorVersion1, Store: validStore, HighestAcceptedRevision: 1, HighestAcceptedSequence: 2},
			DatabaseStore:             validStore,
			DatabaseRevision:          2,
			DatabaseSequenceHighWater: 1,
		},
		{
			Persisted:                 &RestoreAnchor{Version: RestoreAnchorVersion1, Store: validStore},
			DatabaseStore:             validStore,
			DatabaseRevision:          1,
			DatabaseSequenceHighWater: 2,
		},
		{
			Persisted: &RestoreAnchor{
				Version:                 RestoreAnchorVersion1,
				Store:                   CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: math.MaxUint64},
				HighestAcceptedRevision: 5,
			},
			DatabaseStore:    CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: math.MaxUint64 - 1},
			DatabaseRevision: 4,
		},
		{
			Persisted:        &RestoreAnchor{Version: RestoreAnchorVersion1, Store: validStore},
			DatabaseStore:    CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: math.MaxUint64},
			DatabaseRevision: 1,
		},
	}

	for i, check := range tests {
		decision := DecideRestoreAnchor(check)
		if decision.Disposition != RestoreAnchorInvalid || decision.EffectsAllowed {
			t.Errorf("case %d decision = %#v, want invalid and effects denied", i, decision)
		}
	}
}

func TestDecideRestoreAnchorNeverAliasesCallerRecords(t *testing.T) {
	anchor := &RestoreAnchor{
		Version:                 RestoreAnchorVersion1,
		Store:                   CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: 1},
		HighestAcceptedRevision: 2,
		HighestAcceptedSequence: 1,
	}
	decision := DecideRestoreAnchor(RestoreAnchorCheck{
		Persisted:                 anchor,
		DatabaseStore:             anchor.Store,
		DatabaseRevision:          3,
		DatabaseSequenceHighWater: 2,
	})
	if decision.Candidate == nil {
		t.Fatal("Candidate = nil")
	}
	decision.Candidate.Store.StoreUUID = "mutated"
	if anchor.Store.StoreUUID != "store-a" {
		t.Fatalf("decision candidate aliases persisted anchor: %#v", anchor)
	}
}

func TestRestoreAnchorDecisionAdmissionErrorIsTyped(t *testing.T) {
	allowed := RestoreAnchorDecision{Disposition: RestoreAnchorEqual, EffectsAllowed: true}
	if err := allowed.AdmissionError(); err != nil {
		t.Fatalf("allowed AdmissionError = %v, want nil", err)
	}
	denied := RestoreAnchorDecision{
		Disposition:             RestoreAnchorDatabaseRewind,
		RetainedHighestRevision: 9,
		RetainedHighestSequence: 5,
		MinimumRecoveryEpoch:    3,
	}
	err := denied.AdmissionError()
	if !errors.Is(err, ErrRestoreAnchorAdmission) {
		t.Fatalf("denied AdmissionError = %v, want ErrRestoreAnchorAdmission", err)
	}
	var typed *RestoreAnchorDecisionError
	if !errors.As(err, &typed) || typed.Decision != denied {
		t.Fatalf("typed decision error = %#v, want %#v", typed, denied)
	}
}
