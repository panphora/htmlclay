//go:build darwin || linux

package platform

import (
	"net"
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// TestPortOwnerFindsThisProcess is the end-to-end check of the lookup: the listener
// is this test binary, so the process the port owner must name is os.Getpid().
func TestPortOwnerFindsThisProcess(t *testing.T) {
	tool := "ss"
	if runtime.GOOS == "darwin" {
		tool = "lsof"
	}
	if _, err := exec.LookPath(tool); err != nil {
		t.Skip(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	port := ln.Addr().(*net.TCPAddr).Port
	pid, name, ok := PortOwner(port)
	if !ok {
		t.Fatalf("PortOwner(%d) found no owner, want pid %d", port, os.Getpid())
	}
	if pid != os.Getpid() {
		t.Fatalf("PortOwner(%d) = %d %q, want pid %d", port, pid, name, os.Getpid())
	}
}
