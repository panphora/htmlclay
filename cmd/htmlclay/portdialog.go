package main

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"slices"
	"time"

	"github.com/panphora/htmlclay/internal/platform"
)

// defaultPorts is the order a first launch tries: plain numbers people can read and type,
// with no common program using them by default, below every system's ephemeral range
// (Linux 32768-60999, macOS and Windows 49152-65535), none on the browsers' blocked-port
// lists, and more than 100 apart so no single Hyper-V or WSL exclusion block (100 ports)
// covers two. If all are taken, bindRandom picks one.
var defaultPorts = []int{1212, 4545, 9797, 3434, 1111}

// oldDefaultPorts are the 1.13.1 and 1.13.2 candidates. A config that remembers one moves
// to defaultPorts once, on its next launch.
var oldDefaultPorts = []int{24190, 24390, 24590, 24790, 24990, 25190, 25390, 25590, 25790, 25990}

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
// A first launch, or one remembering an old default port, takes the first free port in the
// candidate list. A later launch that finds its remembered port held asks, because the
// port is part of every bookmark and every page's browser storage, and moving it silently
// would lose both without a word.
func (a *app) startListener() bool {
	remembered := a.rt.cfg.Port()
	upgrading := slices.Contains(oldDefaultPorts, remembered)
	if remembered == 0 || upgrading {
		b := a.bindFirstFree(0)
		if b == nil && upgrading {
			if held, err := bindLoopback(remembered); err == nil {
				b = held
			}
		}
		if b == nil {
			b = a.bindRandom(0)
		}
		if b == nil {
			a.reportNoPort()
			return false
		}
		a.adoptPort(b)
		if upgrading || len(a.rt.cfg.SitePortList()) > 0 {
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
				moved = a.bindRandom(remembered)
			}
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

// randomPortTries is how many random ports a launch tries once every candidate is taken.
const randomPortTries = 20

// randomPort picks the last-resort port: 10000-32767, so below every system's ephemeral
// range and clear of the low ports programs take by default, and never 10080, which
// browsers refuse to load.
func randomPort() int {
	for {
		if p := 10000 + rand.IntN(32768-10000); p != 10080 {
			return p
		}
	}
}

// bindRandom is the fallback once every candidate is taken. The port it finds is saved
// like any other, so the addresses stay the same from then on.
func (a *app) bindRandom(skip int) *boundPort {
	pick := randomPort
	if a.rt.randomPort != nil {
		pick = a.rt.randomPort
	}
	for range randomPortTries {
		p := pick()
		if p != 0 && p == skip {
			continue
		}
		b, err := bindLoopback(p)
		if err == nil {
			a.rt.logger.Printf("Every candidate port was taken; using random port %d", b.port)
			return b
		}
		a.rt.logger.Printf("Random port %d is not available: %v", p, err)
	}
	return nil
}

func (a *app) reportNoPort() {
	a.rt.logger.Printf("No port could be bound; tried %v and %d random ports", a.rt.portCandidates, randomPortTries)
	msg := fmt.Sprintf("HTML Clay could not find a free port. It tried %v and %d random ports on this computer, and every one was in use.", a.rt.portCandidates, randomPortTries)
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
		Handler:           a.countWrites(http.HandlerFunc(a.serveHost)),
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

// announceHostnames is the one-time notice for a config upgraded from per-folder ports or
// from the old default port: the addresses changed, and page data does not follow.
func (a *app) announceHostnames(port int) {
	msg := fmt.Sprintf("HTML Clay now serves every folder from port %d, each at its own address, such as http://htmlclay.localhost:%d/.\n\n"+
		"Bookmarks to older addresses may stop working, and anything a page stored in the browser (localStorage) under an old address is not carried over.", port, port)
	go func() {
		if err := a.informUser("HTML Clay", msg); err != nil {
			a.rt.logger.Printf("Could not show the hostname notice: %v", err)
		}
	}()
}

func (a *app) lookupPortOwner(port int) (int, string, bool) {
	var pid int
	var name string
	var known bool
	if a.rt.portOwner != nil {
		pid, name, known = a.rt.portOwner(port)
	} else {
		pid, name, known = platform.PortOwner(port)
	}
	if known && (pid <= 1 || name == "systemd" || name == "launchd" || name == "init") {
		a.rt.logger.Printf("Port %d is held by system process %s (PID %d); not offering to quit it", port, name, pid)
		return pid, name, false
	}
	return pid, name, known
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
