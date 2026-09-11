//go:build windows

package helper

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func launchArgv(path string) ([]string, *syscall.SysProcAttr) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".bat" && ext != ".cmd" {
		return []string{path}, nil
	}
	interpreter := os.Getenv("COMSPEC")
	if interpreter == "" {
		interpreter = "cmd.exe"
	}
	argv := []string{interpreter, "/d", "/s", "/c", path}
	cmdLine := fmt.Sprintf(`%s /d /s /c ""%s""`, syscall.EscapeArg(interpreter), path)
	return argv, &syscall.SysProcAttr{CmdLine: cmdLine}
}

var childStop os.Signal = os.Kill
