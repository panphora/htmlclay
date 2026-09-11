//go:build !windows

package helper

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// loginPathBudget is how long a login shell gets to print its PATH, and
// loginPathWaitDelay is how long after that its pipes are given to drain.
//
// The second one is not decoration. Cancelling the context kills the shell, but
// Output() reads stdout to EOF, and a profile that backgrounds anything
// inheriting stdout keeps that pipe open after its parent is gone, so the read
// blocks well past the budget. Measured at 5.2s against a 3s budget with one
// backgrounded sleeper. WaitDelay is what closes the pipes anyway. It matters
// more here than the usual leaked-descendant case, because this runs under
// loginPathCache's lock, so every helper waiting for a resolved PATH waits with
// it.
const (
	loginPathBudget    = 3 * time.Second
	loginPathWaitDelay = time.Second
)

var resolveLoginPath = func() string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), loginPathBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-l", "-c", `printf %s "$PATH"`)
	cmd.WaitDelay = loginPathWaitDelay
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
