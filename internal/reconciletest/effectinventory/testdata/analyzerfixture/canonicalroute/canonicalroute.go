// Package canonicalroute loads the production raw-process guard closure and
// the authored route-hop fixture through one small analysis root.
package canonicalroute

import (
	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/processgroup"
	"github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/routehops"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// Seed keeps the canonical raw-process vehicles and route-hop fixture in the
// loaded production dependency closure.
func Seed() {
	_ = pidutil.SignalProcess
	_ = processgroup.SignalGroup
	_ = runtime.SignalProcessGroup
	_ = proctable.KillByPID
	routehops.DuplicateOwner()
}
