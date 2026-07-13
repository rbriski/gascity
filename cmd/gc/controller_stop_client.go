package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

const (
	controllerStopDialTimeout   = 2 * time.Second
	controllerStopWriteTimeout  = 2 * time.Second
	controllerStopReadTimeout   = 10 * time.Second
	controllerStopResponseLimit = 64
)

var (
	errControllerStopDefinitePreEntryUnavailable = errors.New("controller stop definitely unavailable before entry")
	errControllerStopMayHaveEntered              = errors.New("controller stop may have entered")
)

type controllerStopOutcome uint8

const (
	controllerStopOutcomeInvalid controllerStopOutcome = iota
	controllerStopAcknowledged
	controllerStopDefinitePreEntryUnavailable
	controllerStopMayHaveEntered
)

func (o controllerStopOutcome) String() string {
	switch o {
	case controllerStopOutcomeInvalid:
		return "invalid"
	case controllerStopAcknowledged:
		return "acknowledged"
	case controllerStopDefinitePreEntryUnavailable:
		return "definite_pre_entry_unavailable"
	case controllerStopMayHaveEntered:
		return "may_have_entered"
	default:
		return fmt.Sprintf("controller_stop_outcome(%d)", o)
	}
}

type controllerStopResult struct {
	outcome    controllerStopOutcome
	err        error
	socketPath string
	socketInfo os.FileInfo
}

func (r controllerStopResult) failClosedError() error {
	if r.err != nil {
		return r.err
	}
	return fmt.Errorf("controller stop returned non-authoritative outcome %s", r.outcome)
}

type controllerStopTransportError struct {
	outcome controllerStopOutcome
	op      string
	err     error
}

func (e controllerStopTransportError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("controller stop %s: %s", e.outcome, e.op)
	}
	return fmt.Sprintf("controller stop %s: %s: %v", e.outcome, e.op, e.err)
}

func (e controllerStopTransportError) Unwrap() error {
	return e.err
}

func (e controllerStopTransportError) Is(target error) bool {
	return (target == errControllerStopDefinitePreEntryUnavailable && e.outcome == controllerStopDefinitePreEntryUnavailable) ||
		(target == errControllerStopMayHaveEntered && e.outcome == controllerStopMayHaveEntered)
}

type controllerStopClient struct {
	stat               func(string) (os.FileInfo, error)
	dial               func(network, address string, timeout time.Duration) (net.Conn, error)
	dialTimeout        time.Duration
	writeTimeout       time.Duration
	readTimeout        time.Duration
	completionDeadline time.Time
	now                func() time.Time
}

func sendControllerStop(cityPath string, force bool) controllerStopResult {
	client := controllerStopClient{
		stat:         os.Stat,
		dial:         net.DialTimeout,
		dialTimeout:  controllerStopDialTimeout,
		writeTimeout: controllerStopWriteTimeout,
		readTimeout:  controllerStopReadTimeout,
	}
	return client.stop(cityPath, force)
}

var controllerStopRequestForCommand = sendControllerStop

func sendControllerStopUntil(cityPath string, force bool, deadline time.Time) controllerStopResult {
	client := controllerStopClient{
		stat:               os.Stat,
		dial:               net.DialTimeout,
		dialTimeout:        controllerStopDialTimeout,
		writeTimeout:       controllerStopWriteTimeout,
		readTimeout:        controllerStopReadTimeout,
		completionDeadline: deadline,
		now:                time.Now,
	}
	return client.stop(cityPath, force)
}

var controllerStopRequestUntilForCommand = sendControllerStopUntil

func (c controllerStopClient) stop(cityPath string, force bool) controllerStopResult {
	sockPath := controllerSocketPath(cityPath)
	now := c.now
	if now == nil {
		now = time.Now
	}
	boundedDuration := func(local time.Duration) (time.Duration, bool) {
		if c.completionDeadline.IsZero() {
			return local, true
		}
		remaining := c.completionDeadline.Sub(now())
		if remaining <= 0 {
			return 0, false
		}
		if local <= 0 || local > remaining {
			return remaining, true
		}
		return local, true
	}
	boundedDeadline := func(local time.Duration) (time.Time, bool) {
		current := now()
		if c.completionDeadline.IsZero() {
			return current.Add(local), true
		}
		remaining := c.completionDeadline.Sub(current)
		if remaining <= 0 {
			return time.Time{}, false
		}
		if local > 0 && local < remaining {
			return current.Add(local), true
		}
		return c.completionDeadline, true
	}
	var before os.FileInfo
	classified := func(outcome controllerStopOutcome, op string, err error) controllerStopResult {
		result := classifiedControllerStopResult(outcome, op, err)
		result.socketPath = sockPath
		result.socketInfo = before
		return result
	}

	var err error
	if _, ok := boundedDuration(0); !ok {
		return classified(controllerStopDefinitePreEntryUnavailable, "completion deadline before stating socket", os.ErrDeadlineExceeded)
	}
	before, err = c.stat(sockPath)
	if err != nil {
		return classified(controllerStopDefinitePreEntryUnavailable, "stating socket before dial", err)
	}
	if before == nil {
		return classified(controllerStopDefinitePreEntryUnavailable, "stating socket before dial", errors.New("socket stat returned no identity"))
	}

	dialTimeout, ok := boundedDuration(c.dialTimeout)
	if !ok {
		return classified(controllerStopDefinitePreEntryUnavailable, "completion deadline before dialing socket", os.ErrDeadlineExceeded)
	}
	conn, err := c.dial("unix", sockPath, dialTimeout)
	if err != nil {
		return classified(controllerStopDefinitePreEntryUnavailable, "dialing socket", err)
	}
	defer conn.Close() //nolint:errcheck // the classified exchange result is authoritative
	if _, ok := boundedDuration(0); !ok {
		return classified(controllerStopMayHaveEntered, "completion deadline after dialing socket", os.ErrDeadlineExceeded)
	}

	after, err := c.stat(sockPath)
	if err != nil {
		return classified(controllerStopMayHaveEntered, "stating socket after dial", err)
	}
	if after == nil {
		return classified(controllerStopMayHaveEntered, "stating socket after dial", errors.New("socket stat returned no identity"))
	}
	if !os.SameFile(before, after) {
		return classified(controllerStopMayHaveEntered, "verifying socket identity", errors.New("controller socket changed during dial"))
	}

	writeDeadline, ok := boundedDeadline(c.writeTimeout)
	if !ok {
		return classified(controllerStopMayHaveEntered, "completion deadline before writing command", os.ErrDeadlineExceeded)
	}
	if err := conn.SetWriteDeadline(writeDeadline); err != nil {
		return classified(controllerStopMayHaveEntered, "setting write deadline", err)
	}
	command := []byte("stop\n")
	if force {
		command = []byte("stop-force\n")
	}
	n, err := conn.Write(command)
	if err != nil {
		return classified(controllerStopMayHaveEntered, "writing command", err)
	}
	if n != len(command) {
		return classified(controllerStopMayHaveEntered, "writing command", fmt.Errorf("short write: wrote %d of %d bytes", n, len(command)))
	}

	readDeadline, ok := boundedDeadline(c.readTimeout)
	if !ok {
		return classified(controllerStopMayHaveEntered, "completion deadline before reading acknowledgement", os.ErrDeadlineExceeded)
	}
	if err := conn.SetReadDeadline(readDeadline); err != nil {
		return classified(controllerStopMayHaveEntered, "setting read deadline", err)
	}
	reply, err := readControllerStopReply(conn)
	if err != nil {
		return classified(controllerStopMayHaveEntered, "reading acknowledgement", err)
	}
	if !bytes.Equal(reply, []byte("ok\n")) {
		return classified(controllerStopMayHaveEntered, "validating acknowledgement", fmt.Errorf("unexpected response %q", reply))
	}
	return controllerStopResult{
		outcome:    controllerStopAcknowledged,
		socketPath: sockPath,
		socketInfo: before,
	}
}

func readControllerStopReply(r io.Reader) ([]byte, error) {
	reply, err := io.ReadAll(io.LimitReader(r, controllerStopResponseLimit+1))
	if err != nil {
		return nil, err
	}
	if len(reply) > controllerStopResponseLimit {
		return nil, fmt.Errorf("response exceeds %d-byte limit", controllerStopResponseLimit)
	}
	return reply, nil
}

func classifiedControllerStopResult(outcome controllerStopOutcome, op string, err error) controllerStopResult {
	return controllerStopResult{
		outcome: outcome,
		err: controllerStopTransportError{
			outcome: outcome,
			op:      op,
			err:     err,
		},
	}
}
