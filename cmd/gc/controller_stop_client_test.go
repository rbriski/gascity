package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestControllerStopResultZeroValueIsNotAcknowledged(t *testing.T) {
	t.Parallel()

	var result controllerStopResult
	if result.outcome == controllerStopAcknowledged {
		t.Fatal("zero-value controller stop result must not authorize acknowledged cleanup")
	}
	if result.outcome != controllerStopOutcomeInvalid {
		t.Fatalf("zero-value outcome = %v, want invalid", result.outcome)
	}
}

func TestGenericControllerCommandRejectsStopTransport(t *testing.T) {
	t.Parallel()

	for _, command := range []string{
		"stop",
		"stop-force",
		"stop\nanything",
		"stop-force\nanything",
		"stop\r",
		"stop-force\r",
	} {
		t.Run(command, func(t *testing.T) {
			response, err := sendControllerCommandWithTimeouts(t.TempDir(), command, time.Second, time.Second, time.Second)
			if response != nil {
				t.Fatalf("response = %q, want nil", response)
			}
			if !errors.Is(err, errControllerStopRequiresTypedClient) {
				t.Fatalf("errors.Is(err, errControllerStopRequiresTypedClient) = false: %v", err)
			}
		})
	}
}

func TestControllerStopClientClassifiesPreEntryFailures(t *testing.T) {
	t.Parallel()

	statErr := errors.New("stat failed")
	dialErr := errors.New("dial failed")
	tests := []struct {
		name        string
		stat        func(string) (os.FileInfo, error)
		dial        func(string, string, time.Duration) (net.Conn, error)
		wantCause   error
		wantDials   int
		wantOutcome controllerStopOutcome
	}{
		{
			name: "socket cannot be stated",
			stat: func(string) (os.FileInfo, error) {
				return nil, statErr
			},
			wantCause:   statErr,
			wantOutcome: controllerStopDefinitePreEntryUnavailable,
		},
		{
			name: "dial fails",
			stat: func(string) (os.FileInfo, error) {
				return statFixtureInfo(t, "socket-before"), nil
			},
			dial: func(string, string, time.Duration) (net.Conn, error) {
				return nil, dialErr
			},
			wantCause:   dialErr,
			wantDials:   1,
			wantOutcome: controllerStopDefinitePreEntryUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dials := 0
			dial := tt.dial
			if dial == nil {
				dial = func(string, string, time.Duration) (net.Conn, error) {
					t.Fatal("dial called after pre-dial stat failure")
					return nil, errors.New("unreachable")
				}
			}
			client := controllerStopClient{
				stat: tt.stat,
				dial: func(network, address string, timeout time.Duration) (net.Conn, error) {
					dials++
					return dial(network, address, timeout)
				},
				dialTimeout:  time.Second,
				writeTimeout: time.Second,
				readTimeout:  time.Second,
			}

			got := client.stop(t.TempDir(), false)

			if got.outcome != tt.wantOutcome {
				t.Fatalf("outcome = %v, want %v (err=%v)", got.outcome, tt.wantOutcome, got.err)
			}
			if got.err == nil {
				t.Fatal("err = nil, want classified transport error")
			}
			if !errors.Is(got.err, errControllerStopDefinitePreEntryUnavailable) {
				t.Fatalf("errors.Is(err, errControllerStopDefinitePreEntryUnavailable) = false: %v", got.err)
			}
			if errors.Is(got.err, errControllerStopMayHaveEntered) {
				t.Fatalf("pre-entry error also matches ambiguity sentinel: %v", got.err)
			}
			if !errors.Is(got.err, tt.wantCause) {
				t.Fatalf("errors.Is(err, cause) = false: %v", got.err)
			}
			if dials != tt.wantDials {
				t.Fatalf("dial calls = %d, want %d", dials, tt.wantDials)
			}
		})
	}
}

func TestControllerStopClientAcknowledgesOnlyCompleteExactReply(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		force   bool
		reads   []scriptedStopRead
		wantCmd string
	}{
		{
			name:    "ordinary stop",
			reads:   []scriptedStopRead{{data: []byte("ok\n")}, {err: io.EOF}},
			wantCmd: "stop\n",
		},
		{
			name:  "force stop",
			force: true,
			reads: []scriptedStopRead{
				{data: []byte("o")},
				{data: []byte("k\n")},
				{err: io.EOF},
			},
			wantCmd: "stop-force\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := &scriptedStopConn{reads: append([]scriptedStopRead(nil), tt.reads...)}
			client, socketInfo := sameIdentityStopClient(t, conn)
			cityPath := t.TempDir()

			got := client.stop(cityPath, tt.force)

			if got.outcome != controllerStopAcknowledged || got.err != nil {
				t.Fatalf("stop result = {%v, %v}, want acknowledged", got.outcome, got.err)
			}
			if got.socketPath != controllerSocketPath(cityPath) {
				t.Fatalf("socket witness path = %q, want %q", got.socketPath, controllerSocketPath(cityPath))
			}
			if got.socketInfo == nil || !os.SameFile(got.socketInfo, socketInfo) {
				t.Fatal("acknowledged result did not retain its pre-dial socket identity")
			}
			if gotCommand := conn.writes.String(); gotCommand != tt.wantCmd {
				t.Fatalf("command = %q, want %q", gotCommand, tt.wantCmd)
			}
			if conn.writeDeadlines != 1 || conn.readDeadlines != 1 {
				t.Fatalf("deadline calls = write:%d read:%d, want 1 each", conn.writeDeadlines, conn.readDeadlines)
			}
			if !conn.closed {
				t.Fatal("connection was not closed")
			}
		})
	}
}

func TestControllerStopClientClassifiesEveryPostDialFailureAsMayHaveEntered(t *testing.T) {
	t.Parallel()

	readReset := errors.New("connection reset")
	writeErr := errors.New("write failed")
	deadlineErr := errors.New("deadline failed")
	postStatErr := errors.New("post-dial stat failed")

	tests := []struct {
		name         string
		conn         *scriptedStopConn
		postStatErr  error
		mismatchInfo bool
		wantWritten  int
	}{
		{
			name:         "socket identity changed",
			conn:         &scriptedStopConn{reads: []scriptedStopRead{{data: []byte("ok\n")}, {err: io.EOF}}},
			mismatchInfo: true,
		},
		{
			name:        "socket cannot be stated after dial",
			conn:        &scriptedStopConn{},
			postStatErr: postStatErr,
		},
		{
			name: "write deadline failure",
			conn: &scriptedStopConn{setWriteDeadlineErr: deadlineErr},
		},
		{
			name:        "short write",
			conn:        &scriptedStopConn{writeLimit: 2},
			wantWritten: 2,
		},
		{
			name:        "write error",
			conn:        &scriptedStopConn{writeErr: writeErr},
			wantWritten: len("stop\n"),
		},
		{
			name:        "read deadline failure",
			conn:        &scriptedStopConn{setReadDeadlineErr: deadlineErr},
			wantWritten: len("stop\n"),
		},
		{
			name:        "empty eof",
			conn:        &scriptedStopConn{reads: []scriptedStopRead{{err: io.EOF}}},
			wantWritten: len("stop\n"),
		},
		{
			name:        "partial acknowledgement",
			conn:        &scriptedStopConn{reads: []scriptedStopRead{{data: []byte("ok")}, {err: io.EOF}}},
			wantWritten: len("stop\n"),
		},
		{
			name:        "malformed acknowledgement",
			conn:        &scriptedStopConn{reads: []scriptedStopRead{{data: []byte("no\n")}, {err: io.EOF}}},
			wantWritten: len("stop\n"),
		},
		{
			name:        "extra acknowledgement bytes",
			conn:        &scriptedStopConn{reads: []scriptedStopRead{{data: []byte("ok\nextra")}, {err: io.EOF}}},
			wantWritten: len("stop\n"),
		},
		{
			name:        "connection reset",
			conn:        &scriptedStopConn{reads: []scriptedStopRead{{err: readReset}}},
			wantWritten: len("stop\n"),
		},
		{
			name:        "read timeout",
			conn:        &scriptedStopConn{reads: []scriptedStopRead{{err: scriptedStopTimeoutError{}}}},
			wantWritten: len("stop\n"),
		},
		{
			name:        "complete acknowledgement followed by reset",
			conn:        &scriptedStopConn{reads: []scriptedStopRead{{data: []byte("ok\n")}, {err: readReset}}},
			wantWritten: len("stop\n"),
		},
		{
			name:        "complete acknowledgement followed by timeout",
			conn:        &scriptedStopConn{reads: []scriptedStopRead{{data: []byte("ok\n")}, {err: scriptedStopTimeoutError{}}}},
			wantWritten: len("stop\n"),
		},
		{
			name:        "reply exceeds bound",
			conn:        &scriptedStopConn{reads: []scriptedStopRead{{data: bytes.Repeat([]byte("x"), controllerStopResponseLimit+1)}, {err: io.EOF}}},
			wantWritten: len("stop\n"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := statFixtureInfo(t, "before")
			after := before
			if tt.mismatchInfo {
				after = statFixtureInfo(t, "replacement")
			}
			stats := 0
			client := controllerStopClient{
				stat: func(string) (os.FileInfo, error) {
					stats++
					if stats == 1 {
						return before, nil
					}
					return after, tt.postStatErr
				},
				dial: func(string, string, time.Duration) (net.Conn, error) {
					return tt.conn, nil
				},
				dialTimeout:  time.Second,
				writeTimeout: time.Second,
				readTimeout:  time.Second,
			}

			got := client.stop(t.TempDir(), false)

			if got.outcome != controllerStopMayHaveEntered {
				t.Fatalf("outcome = %v, want may-have-entered (err=%v)", got.outcome, got.err)
			}
			if got.err == nil || !errors.Is(got.err, errControllerStopMayHaveEntered) {
				t.Fatalf("errors.Is(err, errControllerStopMayHaveEntered) = false: %v", got.err)
			}
			if errors.Is(got.err, errControllerStopDefinitePreEntryUnavailable) {
				t.Fatalf("ambiguous error also matches pre-entry sentinel: %v", got.err)
			}
			if stats != 2 {
				t.Fatalf("stat calls = %d, want 2", stats)
			}
			if !tt.conn.closed {
				t.Fatal("connection was not closed")
			}
			if got := tt.conn.writes.Len(); got != tt.wantWritten {
				t.Fatalf("bytes written = %d, want %d", got, tt.wantWritten)
			}
		})
	}
}

func TestControllerStopClientRealUnixReplacementAfterDialIsAmbiguous(t *testing.T) {
	cityPath := shortSocketTempDir(t, "gc-stop-replacement-")
	for len(filepath.Join(normalizePathForCompare(cityPath), ".gc", "controller.sock")) <= controllerSocketPathLimit {
		cityPath = filepath.Join(cityPath, "long-controller-path-segment")
	}
	sockPath := controllerSocketPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	listenerA, err := net.ListenUnix("unix", &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listenerA.SetUnlinkOnClose(false)
	t.Cleanup(func() {
		_ = listenerA.Close()
		_ = os.Remove(sockPath)
	})
	infoA, err := os.Stat(sockPath)
	if err != nil {
		t.Fatal(err)
	}

	var listenerB *net.UnixListener
	var acceptedA net.Conn
	client := controllerStopClient{
		stat: os.Stat,
		dial: func(network, address string, timeout time.Duration) (net.Conn, error) {
			clientA, dialErr := net.DialTimeout(network, address, timeout)
			if dialErr != nil {
				return nil, dialErr
			}
			acceptedA, dialErr = listenerA.Accept()
			if dialErr != nil {
				_ = clientA.Close()
				return nil, dialErr
			}
			if removeErr := os.Remove(sockPath); removeErr != nil {
				_ = clientA.Close()
				_ = acceptedA.Close()
				return nil, removeErr
			}
			listenerB, dialErr = net.ListenUnix("unix", &net.UnixAddr{Name: sockPath, Net: "unix"})
			if dialErr != nil {
				_ = clientA.Close()
				_ = acceptedA.Close()
				return nil, dialErr
			}
			listenerB.SetUnlinkOnClose(false)
			return clientA, nil
		},
		dialTimeout:  time.Second,
		writeTimeout: time.Second,
		readTimeout:  time.Second,
	}

	result := client.stop(cityPath, false)
	if listenerB == nil || acceptedA == nil {
		t.Fatal("dial replacement barrier did not establish both socket owners")
	}
	t.Cleanup(func() {
		_ = acceptedA.Close()
		_ = listenerB.Close()
	})
	infoB, err := os.Stat(sockPath)
	if err != nil {
		t.Fatal(err)
	}

	if result.outcome != controllerStopMayHaveEntered || !errors.Is(result.err, errControllerStopMayHaveEntered) {
		t.Fatalf("stop result = {%v, %v}, want may-have-entered", result.outcome, result.err)
	}
	if result.socketPath != sockPath {
		t.Fatalf("socket witness path = %q, want %q", result.socketPath, sockPath)
	}
	if result.socketInfo == nil || !os.SameFile(result.socketInfo, infoA) {
		t.Fatal("socket witness does not identify the dialed socket")
	}
	if os.SameFile(result.socketInfo, infoB) {
		t.Fatal("socket witness incorrectly identifies the replacement socket")
	}
	if err := acceptedA.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	n, readErr := acceptedA.Read(buf)
	if n != 0 || !errors.Is(readErr, io.EOF) {
		t.Fatalf("dialed controller received %d bytes with error %v, want zero bytes then EOF", n, readErr)
	}
}

func sameIdentityStopClient(t *testing.T, conn net.Conn) (controllerStopClient, os.FileInfo) {
	t.Helper()
	info := statFixtureInfo(t, "same-identity")
	return controllerStopClient{
		stat:         func(string) (os.FileInfo, error) { return info, nil },
		dial:         func(string, string, time.Duration) (net.Conn, error) { return conn, nil },
		dialTimeout:  time.Second,
		writeTimeout: time.Second,
		readTimeout:  time.Second,
	}, info
}

func statFixtureInfo(t *testing.T, name string) os.FileInfo {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

type scriptedStopRead struct {
	data []byte
	err  error
}

type scriptedStopConn struct {
	reads               []scriptedStopRead
	writes              bytes.Buffer
	writeLimit          int
	writeErr            error
	setWriteDeadlineErr error
	setReadDeadlineErr  error
	writeDeadlines      int
	readDeadlines       int
	closed              bool
}

func (c *scriptedStopConn) Read(p []byte) (int, error) {
	if len(c.reads) == 0 {
		return 0, io.EOF
	}
	next := c.reads[0]
	c.reads = c.reads[1:]
	n := copy(p, next.data)
	return n, next.err
}

func (c *scriptedStopConn) Write(p []byte) (int, error) {
	n := len(p)
	if c.writeLimit > 0 && c.writeLimit < n {
		n = c.writeLimit
	}
	_, _ = c.writes.Write(p[:n])
	return n, c.writeErr
}

func (c *scriptedStopConn) Close() error {
	c.closed = true
	return nil
}

func (c *scriptedStopConn) LocalAddr() net.Addr  { return scriptedStopAddr("local") }
func (c *scriptedStopConn) RemoteAddr() net.Addr { return scriptedStopAddr("remote") }
func (c *scriptedStopConn) SetDeadline(time.Time) error {
	return errors.New("unexpected SetDeadline call")
}

func (c *scriptedStopConn) SetWriteDeadline(time.Time) error {
	c.writeDeadlines++
	return c.setWriteDeadlineErr
}

func (c *scriptedStopConn) SetReadDeadline(time.Time) error {
	c.readDeadlines++
	return c.setReadDeadlineErr
}

type scriptedStopAddr string

func (a scriptedStopAddr) Network() string { return "test" }
func (a scriptedStopAddr) String() string  { return string(a) }

type scriptedStopTimeoutError struct{}

func (scriptedStopTimeoutError) Error() string   { return "deadline exceeded" }
func (scriptedStopTimeoutError) Timeout() bool   { return true }
func (scriptedStopTimeoutError) Temporary() bool { return true }
