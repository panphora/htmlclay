//go:build darwin

package platform

import (
	"fmt"
	"os/exec"
	"strings"
)

func notify(title, message string) error {
	script := fmt.Sprintf("display notification %s with title %s",
		appleScriptString(message), appleScriptString(title))
	return exec.Command("osascript", "-e", script).Run()
}

func appleScriptString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
