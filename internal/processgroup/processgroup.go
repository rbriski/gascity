// Package processgroup provides process-group cleanup helpers with an explicit
// direct-process fallback on platforms without POSIX process groups.
package processgroup

import (
	"errors"
	"syscall"
	"time"
)

const defaultPollPeriod = 25 * time.Millisecond

// ErrProcessGroupsUnsupported reports that the host cannot perform a
// process-group-only operation. Callers must not interpret it as successful
// tree cleanup.
var ErrProcessGroupsUnsupported = errors.New("process groups are unsupported on this platform")

// Options configures process-group cleanup.
type Options struct {
	killFn           func(pid int, sig syscall.Signal) error
	currentGroupIDFn func() int
	PollPeriod       time.Duration
}

// NewOptions configures the process operations and polling period used during
// group termination. Nil process operations retain the platform defaults.
func NewOptions(
	killFn func(pid int, sig syscall.Signal) error,
	currentGroupIDFn func() int,
	pollPeriod time.Duration,
) Options {
	return Options{
		killFn:           killFn,
		currentGroupIDFn: currentGroupIDFn,
		PollPeriod:       pollPeriod,
	}
}

func (o Options) pollPeriod() time.Duration {
	if o.PollPeriod > 0 {
		return o.PollPeriod
	}
	return defaultPollPeriod
}
