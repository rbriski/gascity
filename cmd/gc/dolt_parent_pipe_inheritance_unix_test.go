//go:build !windows

package main

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMakeManagedDoltParentPipeNonInheritable(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer reader.Close() //nolint:errcheck
	defer writer.Close() //nolint:errcheck

	if _, err := unix.FcntlInt(reader.Fd(), unix.F_SETFD, 0); err != nil {
		t.Fatalf("clear descriptor flags: %v", err)
	}
	if err := makeManagedDoltParentPipeNonInheritable(reader); err != nil {
		t.Fatalf("makeManagedDoltParentPipeNonInheritable: %v", err)
	}
	flags, err := unix.FcntlInt(reader.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatalf("read descriptor flags: %v", err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("descriptor flags = %#x, want FD_CLOEXEC", flags)
	}
}

func TestMakeManagedDoltParentPipeNonInheritableReportsClosedFile(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer writer.Close() //nolint:errcheck
	if err := reader.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}

	err = makeManagedDoltParentPipeNonInheritable(reader)
	if err == nil {
		t.Fatal("makeManagedDoltParentPipeNonInheritable(closed file) error = nil, want error")
	}
	if !strings.Contains(err.Error(), "descriptor flags") {
		t.Fatalf("makeManagedDoltParentPipeNonInheritable(closed file) error = %q, want descriptor context", err)
	}
}

func TestManagedDoltTestParentDoneReportsInheritanceFailure(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer reader.Close() //nolint:errcheck
	defer writer.Close() //nolint:errcheck

	closedFD, err := unix.Dup(int(reader.Fd()))
	if err != nil {
		t.Fatalf("duplicate parent pipe descriptor: %v", err)
	}
	if err := unix.Close(closedFD); err != nil {
		t.Fatalf("close duplicated parent pipe descriptor: %v", err)
	}

	done, closeDone, err := managedDoltTestParentDone(strconv.Itoa(closedFD))
	if err == nil {
		if closeDone != nil {
			closeDone()
		}
		t.Fatal("managedDoltTestParentDone(closed descriptor) error = nil, want error")
	}
	if done != nil {
		t.Fatalf("managedDoltTestParentDone(closed descriptor) returned non-nil done channel")
	}
	if closeDone != nil {
		t.Fatalf("managedDoltTestParentDone(closed descriptor) returned non-nil close function")
	}
	if !strings.Contains(err.Error(), "protect parent pipe fd") || !strings.Contains(err.Error(), "descriptor flags") {
		t.Fatalf("managedDoltTestParentDone(closed descriptor) error = %q, want inheritance context", err)
	}
}
