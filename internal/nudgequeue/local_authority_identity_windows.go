//go:build windows

package nudgequeue

import (
	"os"
)

// SQLite uses byte-range locks on Windows. The sidecar lifetime lock remains
// the exclusion mechanism there; Windows does not permit the Unix lock-path
// replacement used by the identity-race test.
func acquireLocalNudgeAuthorityIdentity(_ string, _ os.FileInfo) (*os.File, error) {
	return nil, nil
}

func releaseLocalNudgeAuthorityIdentity(_ *os.File) error {
	return nil
}
