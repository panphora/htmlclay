package platform

import (
	"regexp"
	"strconv"
	"strings"
)

// loopbackListener reports whether a listening socket's local address conflicts with a
// bind on 127.0.0.1 or ::1.
func loopbackListener(addr string) bool {
	switch strings.Trim(addr, "[]") {
	case "127.0.0.1", "::1", "0.0.0.0", "::", "*", "localhost":
		return true
	}
	return false
}

// parseLsof reads `lsof -nP -iTCP:<port> -sTCP:LISTEN -Fpcn`: one field per line,
// p<pid> then c<command> per process, then n<address>:<port> per socket.
func parseLsof(out string, port int) (int, string, bool) {
	suffix := ":" + strconv.Itoa(port)
	pid, name := 0, ""
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
			if ok && pid > 0 && loopbackListener(addr) {
				if name == "" {
					name = "PID " + strconv.Itoa(pid)
				}
				return pid, name, true
			}
		}
	}
	return 0, "", false
}

var ssUsers = regexp.MustCompile(`users:\(\("([^"]+)",pid=(\d+)`)

// parseSS reads `ss -Hltnp 'sport = :<port>'`: one listener per line, the local address in
// the fourth column, the owner in users:(("name",pid=N,fd=M)).
func parseSS(out string, port int) (int, string, bool) {
	suffix := ":" + strconv.Itoa(port)
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		addr, ok := strings.CutSuffix(f[3], suffix)
		if !ok || !loopbackListener(addr) {
			continue
		}
		m := ssUsers.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if pid, _ := strconv.Atoi(m[2]); pid > 0 {
			return pid, m[1], true
		}
	}
	return 0, "", false
}

// parseNetTCP reads the PowerShell script's output: pid, process name and local address,
// tab-separated, one listener per line.
func parseNetTCP(out string) (int, string, bool) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(f) != 3 || !loopbackListener(strings.TrimSpace(f[2])) {
			continue
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(f[0]))
		if pid <= 0 {
			continue
		}
		name := strings.TrimSpace(f[1])
		if name == "" {
			name = "PID " + strconv.Itoa(pid)
		}
		return pid, name, true
	}
	return 0, "", false
}
