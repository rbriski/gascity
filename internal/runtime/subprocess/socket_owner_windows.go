//go:build windows

package subprocess

import "os"

func privateSocketDirUID(os.FileInfo) (uint32, bool) {
	// Windows does not expose a Unix UID through FileInfo.Sys. The private
	// Unix-socket fallback therefore remains fail-closed on this platform.
	return 0, false
}
