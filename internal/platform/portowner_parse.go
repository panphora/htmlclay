package platform

import (
	"regexp"
	"strconv"
	"strings"
)

// listenerRank orders a listening socket's local address by how surely it blocks a bind on
// 127.0.0.1 or ::1: 2 for an exact loopback address, 1 for a wildcard (which on macOS can
// coexist with an exact bind, so it is only blamed when nothing exact is listening), 0 for
// anything else, which never conflicts.
func listenerRank(addr string) int {
	switch strings.Trim(addr, "[]") {
	case "127.0.0.1", "::1", "localhost":
		return 2
	case "0.0.0.0", "::", "*":
		return 1
	}
	return 0
}

type listenerOwner struct {
	pid  int
	name string
	rank int
}

// pickOwner returns the first owner with the highest nonzero rank.
func pickOwner(owners []listenerOwner) (int, string, bool) {
	best := listenerOwner{}
	for _, o := range owners {
		if o.rank > best.rank {
			best = o
		}
	}
	if best.rank == 0 {
		return 0, "", false
	}
	if best.name == "" {
		best.name = "PID " + strconv.Itoa(best.pid)
	}
	return best.pid, best.name, true
}

// parseLsof reads `lsof -nP -iTCP:<port> -sTCP:LISTEN -Fpcn`: one field per line,
// p<pid> then c<command> per process, then n<address>:<port> per socket.
func parseLsof(out string, port int) (int, string, bool) {
	suffix := ":" + strconv.Itoa(port)
	pid, name := 0, ""
	var owners []listenerOwner
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
			name = ""
		case 'c':
			name = line[1:]
		case 'n':
			addr, ok := strings.CutSuffix(line[1:], suffix)
			if ok && pid > 0 {
				owners = append(owners, listenerOwner{pid, name, listenerRank(addr)})
			}
		}
	}
	return pickOwner(owners)
}

var ssUsers = regexp.MustCompile(`users:\(\("([^"]+)",pid=(\d+)`)

// parseSS reads `ss -Hltnp 'sport = :<port>'`: one listener per line, the local address in
// the fourth column, the owner in users:(("name",pid=N,fd=M)).
func parseSS(out string, port int) (int, string, bool) {
	suffix := ":" + strconv.Itoa(port)
	var owners []listenerOwner
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		addr, ok := strings.CutSuffix(f[3], suffix)
		if !ok {
			continue
		}
		m := ssUsers.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if pid, _ := strconv.Atoi(m[2]); pid > 0 {
			owners = append(owners, listenerOwner{pid, m[1], listenerRank(addr)})
		}
	}
	return pickOwner(owners)
}

// parseNetTCP reads the PowerShell script's output: pid, process name and local address,
// tab-separated, one listener per line.
func parseNetTCP(out string) (int, string, bool) {
	var owners []listenerOwner
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(f) != 3 {
			continue
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(f[0]))
		if pid <= 0 {
			continue
		}
		name := strings.TrimSpace(f[1])
		owners = append(owners, listenerOwner{pid, name, listenerRank(strings.TrimSpace(f[2]))})
	}
	return pickOwner(owners)
}
