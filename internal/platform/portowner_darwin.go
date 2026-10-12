//go:build darwin

package platform

import (
	"context"
	"os/exec"
	"strconv"
	"time"
)

// lsof ships with macOS at this path.
func portOwner(port int) (int, string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fpcn").Output()
	if err != nil {
		return 0, "", false
	}
	return parseLsof(string(out), port)
}
