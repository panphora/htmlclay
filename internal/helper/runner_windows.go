//go:build windows

package helper

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
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

// Windows has no SIGTERM. What a console program can catch is Ctrl+Break, which
// Go delivers as os.Interrupt, Node and Python as SIGBREAK, .NET as
// CancelKeyPress, and which ends a program that handles none of those. The child
// leads its own process group so the event reaches it and nothing else, and
// GenerateConsoleCtrlEvent only works from a process that shares the child's
// console. A host with a console lets the child inherit it. A host without one
// gives the child an invisible console of its own and attaches to it for the
// send, one at a time, because a process can be attached to a single console.
// Either way os/exec kills the child after WaitDelay if it is still running.
func prepareStop(cmd *exec.Cmd) func() error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	ownConsole := !hostHasConsole()
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
	if ownConsole {
		cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
	}
	return func() error {
		if err := breakChild(uint32(cmd.Process.Pid), ownConsole); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}

func hostHasConsole() bool {
	cp, err := windows.GetConsoleCP()
	return err == nil && cp != 0
}

var (
	consoleMu         sync.Mutex
	kernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procAttachConsole = kernel32.NewProc("AttachConsole")
	procFreeConsole   = kernel32.NewProc("FreeConsole")
)

func breakChild(pid uint32, attach bool) error {
	if !attach {
		return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, pid)
	}
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if r, _, err := procAttachConsole.Call(uintptr(pid)); r == 0 {
		return err
	}
	defer procFreeConsole.Call()
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, pid)
}
