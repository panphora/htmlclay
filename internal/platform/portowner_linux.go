//go:build linux

package platform

import (
	"context"
	"os/exec"
	"strconv"
	"time"
)

// ss is iproute2, present on every mainstream distribution. -H no header, -l listening,
// -t tcp, -n numeric, -p processes (own-user sockets need no root).
func portOwner(port int) (int, string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ss", "-Hltnp", "sport = :"+strconv.Itoa(port)).Output()
	if err != nil {
		return 0, "", false
	}
	return parseSS(string(out), port)
}
