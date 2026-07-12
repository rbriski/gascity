package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/config"
)

// cmdStopBody keeps focused direct-stop tests concise while ensuring the
// production entry point always classifies before loading full config.
func cmdStopBody(cityPath string, cfg *config.City, force bool, stdout, stderr io.Writer) int {
	return cmdStopBodyWithResult(cityPath, cfg, force, controllerStopRequestForCommand(cityPath, force), stdout, stderr)
}

func unregisterCityFromSupervisorWithForce(cityPath string, stdout, stderr io.Writer, commandName string, force bool) (bool, int) {
	return unregisterCityFromSupervisorWithForceResult(cityPath, stdout, stderr, commandName, force).legacy()
}

// tryStopController is retained only as test cleanup shorthand. Production
// stop ownership must consume controllerStopResult without a bool projection.
func tryStopController(cityPath string, stdout io.Writer) bool {
	return tryStopControllerWithForce(cityPath, stdout, false)
}

// tryStopControllerWithForce is retained only for tests that start a real
// controller. Keeping this projection out of production prevents future
// mutation paths from treating acknowledgement ambiguity as fallback authority.
func tryStopControllerWithForce(cityPath string, stdout io.Writer, force bool) bool {
	result := sendControllerStop(cityPath, force)
	if result.outcome != controllerStopAcknowledged {
		return false
	}
	fmt.Fprintln(stdout, "Controller stopping...") //nolint:errcheck // best-effort test output
	return true
}
