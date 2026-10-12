package main

import (
	"net"
	"strconv"
	"strings"
	"testing"
)

func TestBindLoopbackHoldsBothStacks(t *testing.T) {
	b, err := bindLoopback(0)
	if err != nil {
		t.Fatalf("bindLoopback(0): %v", err)
	}
	addr := strconv.Itoa(b.port)

	v6Held := true
	v6, err := net.Listen("tcp6", "[::1]:"+addr)
	switch {
	case err == nil:
		v6.Close()
		b.close()
		t.Fatalf("::1:%s is free while bound: a browser's first connection would go elsewhere", addr)
	case ipv6Unavailable(err):
		// No IPv6 loopback on this machine, so bindLoopback left the v6 half nil and
		// only the v4 half can be asserted.
		v6Held = false
	}

	if _, err := net.Listen("tcp4", "127.0.0.1:"+addr); err == nil {
		b.close()
		t.Fatalf("127.0.0.1:%s is free while bound", addr)
	}
	b.close()

	if v6Held {
		v6After, err := net.Listen("tcp6", "[::1]:"+addr)
		if err != nil {
			t.Fatalf("::1:%s must be free after close: %v", addr, err)
		}
		v6After.Close()
	}
	v4After, err := net.Listen("tcp4", "127.0.0.1:"+addr)
	if err != nil {
		t.Fatalf("127.0.0.1:%s must be free after close: %v", addr, err)
	}
	v4After.Close()
}

func TestBindLoopbackReportsAHeldPort(t *testing.T) {
	v4, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen 127.0.0.1: %v", err)
	}
	defer v4.Close()
	port := v4.Addr().(*net.TCPAddr).Port
	if b, err := bindLoopback(port); err == nil {
		b.close()
		t.Fatalf("bindLoopback(%d) succeeded with 127.0.0.1:%d held", port, port)
	}

	v6, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		if ipv6Unavailable(err) {
			return
		}
		t.Fatalf("listen ::1: %v", err)
	}
	defer v6.Close()
	port6 := v6.Addr().(*net.TCPAddr).Port
	if b, err := bindLoopback(port6); err == nil {
		b.close()
		t.Fatalf("bindLoopback(%d) succeeded with ::1:%d held", port6, port6)
	}
}

func TestPortTakenMessageNamesTheOwner(t *testing.T) {
	known := portTakenMessage(517, 517, "node", true)
	if !strings.Contains(known, "node (PID 517)") {
		t.Errorf("known-owner message does not name the owner: %q", known)
	}
	if !strings.Contains(known, "Quit It and Retry") {
		t.Errorf("known-owner message does not offer the quit: %q", known)
	}
	unknown := portTakenMessage(517, 0, "", false)
	if !strings.Contains(unknown, "could not tell") {
		t.Errorf("unknown-owner message does not say so: %q", unknown)
	}
	if !strings.Contains(unknown, "Hyper-V") {
		t.Errorf("unknown-owner message does not mention the reserved ranges: %q", unknown)
	}
}

// blocker holds a port for the two quit tests. It is only ever closed by the test that
// wants the port free.
func blocker(t *testing.T) (int, *net.TCPListener) {
	t.Helper()
	holder, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return holder.Addr().(*net.TCPAddr).Port, holder.(*net.TCPListener)
}

func TestQuitPortOwnerRevalidatesBeforeEachSignal(t *testing.T) {
	port, holder := blocker(t)
	defer holder.Close()

	t.Run("the owner changed before the first signal", func(t *testing.T) {
		a := buildTestApp(t, t.TempDir(), t.TempDir())
		lookups := 0
		a.rt.portOwner = func(int) (int, string, bool) {
			lookups++
			if lookups == 1 {
				return 4242, "blocker", true
			}
			return 99, "other", true
		}
		var calls []bool
		a.rt.quitProcess = func(pid int, force bool) error {
			calls = append(calls, force)
			return nil
		}
		// The first lookup stands in for the one the dialog was raised with; the
		// revalidation quitPortOwner does itself is the second, and finds someone else.
		if pid, name, known := a.lookupPortOwner(port); pid != 4242 || name != "blocker" || !known {
			t.Fatalf("lookup = %d, %q, %v; want 4242, blocker, true", pid, name, known)
		}
		a.quitPortOwner(port, 4242, "blocker")
		if len(calls) != 0 {
			t.Fatalf("signalled %v, want no signals: the owner is no longer the pid the dialog named", calls)
		}
	})

	t.Run("the owner stays the same", func(t *testing.T) {
		a := buildTestApp(t, t.TempDir(), t.TempDir())
		a.rt.portOwner = func(int) (int, string, bool) { return 4242, "blocker", true }
		var calls []bool
		a.rt.quitProcess = func(pid int, force bool) error {
			calls = append(calls, force)
			return nil
		}
		a.quitPortOwner(port, 4242, "blocker")
		if len(calls) != 2 || calls[0] || !calls[1] {
			t.Fatalf("calls = %v, want [false true]: polite then forced, and only those", calls)
		}
	})
}

func TestQuitPortOwnerSkipsAFreedPort(t *testing.T) {
	port, holder := blocker(t)
	holder.Close()

	a := buildTestApp(t, t.TempDir(), t.TempDir())
	logger, log := diagnosticLog(t)
	a.rt.logger = logger
	a.rt.portOwner = func(int) (int, string, bool) { return 4242, "blocker", true }
	calls := 0
	a.rt.quitProcess = func(pid int, force bool) error {
		calls++
		return nil
	}
	a.quitPortOwner(port, 4242, "blocker")
	if calls != 0 {
		t.Fatalf("quit calls = %d, want 0: a port that came free on its own is never signalled", calls)
	}
	if !strings.Contains(log(), "came free") {
		t.Fatalf("log = %q, want the came-free line", log())
	}
}

// Quitting a system process would take down the user's service manager, so a lookup
// that names one reports no owner at all and the dialog offers a plain Retry.
func TestSystemProcessesAreNeverOfferedForQuitting(t *testing.T) {
	a := buildTestApp(t, t.TempDir(), t.TempDir())
	for _, sys := range []struct {
		pid  int
		name string
	}{{1, "launchd"}, {812, "systemd"}} {
		a.rt.portOwner = func(int) (int, string, bool) { return sys.pid, sys.name, true }
		if pid, name, known := a.lookupPortOwner(24190); known {
			t.Errorf("lookupPortOwner = (%d, %q, %v), want known false for %s", pid, name, known, sys.name)
		}
	}
}
