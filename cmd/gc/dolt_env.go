package main

import (
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/processenv"
)

const (
	doltAdaptiveEncodingEnvKey        = processenv.DoltAdaptiveEncodingEnvKey
	doltAdaptiveEncodingDisabledValue = processenv.DoltAdaptiveEncodingDisabledValue
)

func gcDoltSkip() bool {
	return strings.TrimSpace(os.Getenv("GC_DOLT")) == "skip"
}

func applyDoltAdaptiveEncodingMitigationEnv(env map[string]string) {
	processenv.ApplyDoltAdaptiveEncodingMitigation(env)
}

func withDoltAdaptiveEncodingMitigationEnv(environ []string) []string {
	return processenv.WithDoltAdaptiveEncodingMitigation(environ)
}
