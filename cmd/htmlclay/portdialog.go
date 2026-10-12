package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/panphora/htmlclay/internal/platform"
)

// defaultPorts is the order a launch with no remembered port tries. Below 32768, so
// outside the ephemeral range on Linux (32768-60999), macOS and Windows (49152-65535),
// none of the ports local dev servers take by habit, and 200 apart so no single Hyper-V or
// WSL exclusion range (they come in blocks of 100) covers two. Fixed and in order so two
// machines that both lose 24190 land on the same second choice.
var defaultPorts = []int{24190, 24390, 24590, 24790, 24990, 25190, 25390, 25590, 25790, 25990}

// boundPort is the one port held on both loopback stacks. v6 is nil on a machine with no
// IPv6 loopback.
type boundPort struct {
	port int
	v4   net.Listener
	v6   net.Listener
}

func (b *boundPort) close() {
	if b == nil {
		return
	}
	if b.v4 != nil {
		b.v4.Close()
	}
	if b.v6 != nil {
		b.v6.Close()
	}
}

// bindLoopback binds port on 127.0.0.1 and ::1. Browsers connect to a .localhost name over
// ::1 first, so a port held on one stack only would hand the browser's traffic to whoever
// takes the other; either bind failing with the port in use is the port being taken. ::1
// failing because the machine has no IPv6 loopback is not. port 0 picks a free v4 port and
// then binds the same number on ::1, best effort; that is the test configuration.
func bindLoopback(port int) (*boundPort, error) {
	v4, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	chosen := v4.Addr().(*net.TCPAddr).Port
	v6, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", chosen))
	if err != nil {
		if port == 0 || ipv6Unavailable(err) {
			return &boundPort{port: chosen, v4: v4}, nil
		}
		v4.Close()
		return nil, err
	}
	return &boundPort{port: chosen, v4: v4, v6: v6}, nil
}

type portDecision int

const (
	portQuit portDecision = iota
	portMove
	portRetry
)

// startListener binds the app's port and starts the one HTTP server. It returns false
// when the app should exit: the user chose to, or no port could be bound at all.
//
// A first launch takes the first free port in the candidate list and says nothing. A
// later launch that finds its remembered port held asks, because the port is part of
// every bookmark and every page's browser storage, and moving it silently would lose
// both without a word.
func (a *app) startListener() bool {
	remembered := a.rt.cfg.Port()
	if remembered == 0 {
		b := a.bindFirstFree(0)
		if b == nil {
			a.reportNoPort()
			return false
		}
		a.adoptPort(b)
		if len(a.rt.cfg.SitePortList()) > 0 {
			a.announceHostnames(b.port)
		}
		return true
	}
	for {
		b, err := bindLoopback(remembered)
		if err == nil {
			a.adoptPort(b)
			return true
		}
		pid, name, known := a.lookupPortOwner(remembered)
		a.rt.logger.Printf("Port %d is taken (%v): owner pid=%d name=%q known=%v", remembered, err, pid, name, known)
		switch a.askPortTaken(remembered, pid, name, known) {
		case portRetry:
			if known {
				a.quitPortOwner(remembered, pid, name)
			}
		case portMove:
			moved := a.bindFirstFree(remembered)
			if moved == nil {
				a.reportNoPort()
				return false
			}
			a.adoptPort(moved)
			a.rt.logger.Printf("Port moved: previous=%d current=%d; bookmarks and page data on the old port are lost", remembered, moved.port)
			msg := fmt.Sprintf("HTML Clay now uses port %d instead of %d.\n\nBookmarks with the old port stop working, and anything pages stored in the browser under the old address starts over.", moved.port, remembered)
			go func() {
				if nErr := a.notifyUser("HTML Clay", msg); nErr != nil {
					a.rt.logger.Printf("Could not notify about the port move: %v", nErr)
				}
			}()
			return true
		default:
			a.rt.logger.Printf("Exiting: port %d is taken and nothing was chosen to change", remembered)
			return false
		}
	}
}

// bindFirstFree tries the candidates in order, skipping skip (the port just found taken).
func (a *app) bindFirstFree(skip int) *boundPort {
	for _, p := range a.rt.portCandidates {
		if p != 0 && p == skip {
			continue
		}
		b, err := bindLoopback(p)
		if err == nil {
			return b
		}
		a.rt.logger.Printf("Port %d is not available: %v", p, err)
	}
	return nil
}

func (a *app) reportNoPort() {
	a.rt.logger.Printf("No port could be bound; tried %v", a.rt.portCandidates)
	msg := fmt.Sprintf("HTML Clay could not find a free port. It tried %v on this computer and every one was in use.", a.rt.portCandidates)
	if err := a.notifyUser("HTML Clay can't start", msg); err != nil {
		a.rt.logger.Printf("Could not notify about the missing port: %v", err)
	}
}

// adoptPort records the bound port, saves it when it changed, and starts serving on both
// stacks through serveHost.
func (a *app) adoptPort(b *boundPort) {
	a.rt.bound = b
	a.rt.port = b.port
	if a.rt.cfg.Port() != b.port {
		a.rt.cfg.SetPort(b.port)
		a.saveConfig("choosing the port")
	}
	a.rt.httpServer = &http.Server{
		Handler:           http.HandlerFunc(a.serveHost),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	for _, ln := range []net.Listener{b.v4, b.v6} {
		if ln == nil {
			continue
		}
		go func(ln net.Listener) {
			if err := a.rt.httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				a.rt.logger.Printf("Server error on %s: %v", ln.Addr(), err)
			}
		}(ln)
	}
	a.rt.logger.Printf("Listening on port %d (127.0.0.1 and ::1=%v)", b.port, b.v6 != nil)
}

// announceHostnames is the one-time upgrade notice for a config that still carries
// per-folder ports: the addresses changed shape, old bookmarks redirect, page data does not
// follow.
func (a *app) announceHostnames(port int) {
	msg := fmt.Sprintf("HTML Clay now serves every folder from one port, %d, with its own address per folder, such as http://notes-3f2a.localhost:%d/.\n\n"+
		"Old bookmarks redirect to the new address for as long as their old port is free. Anything a page stored in the browser (localStorage) under an old address is not carried over.", port, port)
	go func() {
		if err := a.informUser("HTML Clay", msg); err != nil {
			a.rt.logger.Printf("Could not show the hostname notice: %v", err)
		}
	}()
}

func (a *app) lookupPortOwner(port int) (int, string, bool) {
	if a.rt.portOwner != nil {
		return a.rt.portOwner(port)
	}
	return platform.PortOwner(port)
}

func (a *app) askPortTaken(port, pid int, name string, known bool) portDecision {
	retry := "Retry"
	if known {
		retry = "Quit It and Retry"
	}
	labels := platform.ConfirmLabels{Allow: "Use a New Port", Always: retry, Deny: "Quit HTML Clay"}
	confirm := platform.Confirm
	if a.rt.confirmPort != nil {
		confirm = a.rt.confirmPort
	}
	choice, err := confirm("HTML Clay can't start", portTakenMessage(port, pid, name, known), labels)
	if err != nil {
		a.rt.logger.Printf("Port dialog error: %v", err)
	}
	switch choice {
	case platform.ConfirmAllowOnce:
		return portMove
	case platform.ConfirmAllowAlways:
		return portRetry
	}
	return portQuit
}

func portTakenMessage(port, pid int, name string, known bool) string {
	var head, retry string
	if known {
		head = fmt.Sprintf("Port %d is in use by %s (PID %d).", port, name, pid)
		retry = fmt.Sprintf("Quit It and Retry: quit %s, then start HTML Clay on port %d as usual.", name, port)
	} else {
		head = fmt.Sprintf("Port %d is in use, and HTML Clay could not tell by which program. On Windows this can also mean the port sits in a range reserved by Hyper-V or WSL.", port)
		retry = "Retry: try the port again once you have freed it."
	}
	return head + "\n\n" +
		fmt.Sprintf("HTML Clay serves your files at addresses ending in .localhost:%d. Your bookmarks, and anything pages have stored in the browser, belong to that port.", port) + "\n\n" +
		retry + "\n" +
		"Use a New Port: start on another port. Bookmarks stop working and page data stored in the browser starts over.\n" +
		"Quit HTML Clay: change nothing and try again later."
}

// quitPortOwner asks the owner to quit and waits for the port to free, politely first and
// then by force. The pid came from a lookup made before a dialog that can stay open for
// two minutes, so right before each signal the owner is looked up again and must still be
// the same pid and name, and a port that has come free on its own is never signalled.
// The caller re-binds; this only buys time.
func (a *app) quitPortOwner(port, pid int, name string) {
	quit := platform.QuitProcess
	if a.rt.quitProcess != nil {
		quit = a.rt.quitProcess
	}
	for _, force := range []bool{false, true} {
		if b, err := bindLoopback(port); err == nil {
			b.close()
			a.rt.logger.Printf("Port %d came free before %s (PID %d) was signalled", port, name, pid)
			return
		}
		nowPid, nowName, known := a.lookupPortOwner(port)
		if !known || nowPid != pid || nowName != name {
			a.rt.logger.Printf("Port %d is no longer held by %s (PID %d) but by pid=%d name=%q known=%v; not signalling", port, name, pid, nowPid, nowName, known)
			return
		}
		if err := quit(pid, force); err != nil {
			a.rt.logger.Printf("Could not ask %s (PID %d) to quit (force=%v): %v", name, pid, force, err)
		}
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			if b, err := bindLoopback(port); err == nil {
				b.close()
				a.rt.logger.Printf("%s (PID %d) released port %d (force=%v)", name, pid, port, force)
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// saveConfig persists the config and logs a failure with what was being saved.
func (a *app) saveConfig(what string) {
	if err := a.rt.cfg.Save(); err != nil {
		a.rt.logger.Printf("Could not save config after %s: %v", what, err)
	}
}
