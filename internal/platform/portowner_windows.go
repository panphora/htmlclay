//go:build windows

package platform

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"time"
)

const portOwnerScript = "$ErrorActionPreference = 'SilentlyContinue'; " +
	"Get-NetTCPConnection -State Listen -LocalPort ([int]$env:HTMLCLAY_PORT) | ForEach-Object { " +
	"$p = Get-Process -Id $_.OwningProcess; " +
	"Write-Output (\"{0}`t{1}`t{2}\" -f $_.OwningProcess, $p.ProcessName, $_.LocalAddress) }"

func portOwner(port int) (int, string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", portOwnerScript)
	noConsoleWindow(cmd)
	cmd.Env = append(os.Environ(), "HTMLCLAY_PORT="+strconv.Itoa(port))
	out, err := cmd.Output()
	if err != nil {
		return 0, "", false
	}
	return parseNetTCP(string(out))
}
