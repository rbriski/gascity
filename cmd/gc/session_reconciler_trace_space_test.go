package main

import "testing"

func TestSessionReconcilerTraceSpaceIsLowPreservesHysteresis(t *testing.T) {
	tests := []struct {
		name       string
		available  uint64
		alreadyLow bool
		want       bool
	}{
		{
			name:      "fresh below entry threshold",
			available: sessionReconcilerTraceLowSpaceMinFree - 1,
			want:      true,
		},
		{
			name:      "fresh at entry threshold",
			available: sessionReconcilerTraceLowSpaceMinFree,
		},
		{
			name:      "fresh between thresholds",
			available: sessionReconcilerTraceLowSpaceExitFree - 1,
		},
		{
			name:       "low between thresholds",
			available:  sessionReconcilerTraceLowSpaceMinFree,
			alreadyLow: true,
			want:       true,
		},
		{
			name:       "low below exit threshold",
			available:  sessionReconcilerTraceLowSpaceExitFree - 1,
			alreadyLow: true,
			want:       true,
		},
		{
			name:       "low at exit threshold",
			available:  sessionReconcilerTraceLowSpaceExitFree,
			alreadyLow: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sessionReconcilerTraceSpaceIsLow(tt.available, tt.alreadyLow); got != tt.want {
				t.Fatalf("sessionReconcilerTraceSpaceIsLow(%d, %t) = %t, want %t", tt.available, tt.alreadyLow, got, tt.want)
			}
		})
	}
}
