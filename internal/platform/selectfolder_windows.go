//go:build windows

package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ole32                           = windows.NewLazySystemDLL("ole32.dll")
	shell32                         = windows.NewLazySystemDLL("shell32.dll")
	procCoCreateInstance            = ole32.NewProc("CoCreateInstance")
	procSHCreateItemFromParsingName = shell32.NewProc("SHCreateItemFromParsingName")
	procPostMessageW                = user32.NewProc("PostMessageW")
)

var (
	clsidFileOpenDialog = windows.GUID{Data1: 0xDC1C5A9C, Data2: 0xE88A, Data3: 0x4DDE, Data4: [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	iidFileOpenDialog   = windows.GUID{Data1: 0xD57C7288, Data2: 0xD4AD, Data3: 0x4768, Data4: [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60}}
	iidShellItem        = windows.GUID{Data1: 0x43826D1E, Data2: 0xE718, Data3: 0x42EE, Data4: [8]byte{0xBC, 0x55, 0xA1, 0xE2, 0x61, 0xC3, 0x7B, 0xFE}}
)

// Vtable slots: IUnknown (0-2), IModalWindow::Show (3), then IFileDialog. The
// last one is IShellItem::GetDisplayName.
const (
	slotRelease        = 2
	slotShow           = 3
	slotSetOptions     = 9
	slotGetOptions     = 10
	slotSetFolder      = 12
	slotSetTitle       = 17
	slotGetResult      = 20
	slotGetDisplayName = 5
)

// errNoFileDialog means the modern dialog could not be set up, before anything
// was shown, so falling back to the legacy one raises only one dialog.
var errNoFileDialog = errors.New("file dialog unavailable")

// selectFolder shows the Explorer-style folder picker, opening in the home
// folder. The FolderBrowserDialog it replaces is the legacy tree, where the
// home folder sits below every known folder and the "Home" node cannot be
// chosen. That one stays as the fallback for a machine where the COM dialog
// will not start.
func selectFolder(prompt string) (string, bool, error) {
	dir, ok, err := pickFolder(prompt)
	if errors.Is(err, errNoFileDialog) {
		return selectFolderLegacy(prompt)
	}
	return dir, ok, err
}

func vtableFn(obj unsafe.Pointer, slot int) uintptr {
	vtbl := *(*unsafe.Pointer)(obj)
	return *(*uintptr)(unsafe.Add(vtbl, slot*int(unsafe.Sizeof(uintptr(0)))))
}

func comRelease(obj unsafe.Pointer) {
	syscall.SyscallN(vtableFn(obj, slotRelease), uintptr(obj))
}

func failed(hr uintptr) bool { return int32(hr) < 0 }

func pickFolder(prompt string) (string, bool, error) {
	// COM objects in a single-threaded apartment belong to the thread that made
	// them, and findDialog looks the window up by that thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	const coinitApartmentThreaded = 0x2
	const coinitDisableOLE1DDE = 0x4
	const sFalse = syscall.Errno(1)
	if err := windows.CoInitializeEx(0, coinitApartmentThreaded|coinitDisableOLE1DDE); err != nil && err != sFalse {
		return "", false, errNoFileDialog
	}
	defer windows.CoUninitialize()

	const clsctxInprocServer = 0x1
	var dlg unsafe.Pointer
	hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidFileOpenDialog)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidFileOpenDialog)), uintptr(unsafe.Pointer(&dlg)))
	if failed(hr) || dlg == nil {
		return "", false, errNoFileDialog
	}
	defer comRelease(dlg)

	var opts uint32
	if hr, _, _ := syscall.SyscallN(vtableFn(dlg, slotGetOptions), uintptr(dlg), uintptr(unsafe.Pointer(&opts))); failed(hr) {
		return "", false, errNoFileDialog
	}
	const fosPickFolders = 0x20
	const fosForceFileSystem = 0x40
	const fosPathMustExist = 0x800
	if hr, _, _ := syscall.SyscallN(vtableFn(dlg, slotSetOptions), uintptr(dlg), uintptr(opts|fosPickFolders|fosForceFileSystem|fosPathMustExist)); failed(hr) {
		return "", false, errNoFileDialog
	}

	title, err := windows.UTF16PtrFromString(pickerTitle(prompt))
	if err != nil {
		return "", false, errNoFileDialog
	}
	syscall.SyscallN(vtableFn(dlg, slotSetTitle), uintptr(dlg), uintptr(unsafe.Pointer(title)))
	if home, err := os.UserHomeDir(); err == nil {
		if start := shellItem(home); start != nil {
			syscall.SyscallN(vtableFn(dlg, slotSetFolder), uintptr(dlg), uintptr(start))
			comRelease(start)
		}
	}

	done := make(chan struct{})
	defer close(done)
	go watchPicker(title, windows.GetCurrentThreadId(), done)

	const errorCancelled = 0x800704C7
	hr, _, _ = syscall.SyscallN(vtableFn(dlg, slotShow), uintptr(dlg), 0)
	if uint32(hr) == errorCancelled {
		return "", false, nil
	}
	if failed(hr) {
		return "", false, fmt.Errorf("folder picker failed: HRESULT %#x", uint32(hr))
	}

	var item unsafe.Pointer
	if hr, _, _ := syscall.SyscallN(vtableFn(dlg, slotGetResult), uintptr(dlg), uintptr(unsafe.Pointer(&item))); failed(hr) || item == nil {
		return "", false, fmt.Errorf("folder picker failed: no result, HRESULT %#x", uint32(hr))
	}
	defer comRelease(item)
	const sigdnFileSysPath = 0x80058000
	var name *uint16
	if hr, _, _ := syscall.SyscallN(vtableFn(item, slotGetDisplayName), uintptr(item), sigdnFileSysPath, uintptr(unsafe.Pointer(&name))); failed(hr) || name == nil {
		return "", false, fmt.Errorf("folder picker failed: no path, HRESULT %#x", uint32(hr))
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(name))
	dir := windows.UTF16PtrToString(name)
	if dir == "" {
		return "", false, nil
	}
	return dir, true, nil
}

// shellItem is the IShellItem for path, or nil when Windows cannot parse it.
func shellItem(path string) unsafe.Pointer {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil
	}
	var item unsafe.Pointer
	hr, _, _ := procSHCreateItemFromParsingName.Call(uintptr(unsafe.Pointer(p)), 0,
		uintptr(unsafe.Pointer(&iidShellItem)), uintptr(unsafe.Pointer(&item)))
	if failed(hr) {
		return nil
	}
	return item
}

// pickerTitle is the prompt's first sentence. The dialog has no room for a
// description, and the whole prompt would not fit its title bar.
func pickerTitle(prompt string) string {
	if i := strings.Index(prompt, ". "); i > 0 {
		return prompt[:i]
	}
	return prompt
}

// watchPicker brings the picker to the front, as raiseBox does for message
// boxes, then closes it if it is still open at pickerTimeout. Closing it makes
// Show return as a cancel.
func watchPicker(title *uint16, owner uint32, done <-chan struct{}) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	box := findDialog(title, owner, done)
	if box == 0 {
		return
	}
	bringToFront(box)
	select {
	case <-done:
	case <-time.After(pickerTimeout):
		const wmClose = 0x10
		procPostMessageW.Call(box, wmClose, 0, 0)
	}
}

// selectFolderLegacy shows a FolderBrowserDialog via PowerShell. On OK it prints the
// selected path and yields ok=true; a cancel prints nothing and exits zero, so an
// empty output is a clean cancel (ok=false, no error); a non-zero exit is a genuine
// launch failure and is surfaced as an error rather than a silent cancel.
//
// The prompt is passed as an environment variable and referenced with $env:VAR
// inside the script, never spliced into the script text (see confirmDialog for the
// PowerShell quoting hazard this avoids).
func selectFolderLegacy(prompt string) (string, bool, error) {
	const script = "$ErrorActionPreference = 'Stop'; " +
		"Add-Type -AssemblyName System.Windows.Forms; " +
		"$d = New-Object System.Windows.Forms.FolderBrowserDialog; " +
		"$d.Description = $env:HTMLCLAY_DIALOG_PROMPT; " +
		"if ($d.ShowDialog() -eq 'OK') { Write-Output $d.SelectedPath }"

	ctx, cancel := context.WithTimeout(context.Background(), pickerTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	noConsoleWindow(cmd)
	cmd.Env = append(cmd.Environ(), "HTMLCLAY_DIALOG_PROMPT="+prompt)
	out, err := cmd.Output()
	if err != nil {
		return "", false, fmt.Errorf("folder picker failed: %w", err)
	}
	// Trim only line endings, so a folder name with legitimate trailing spaces is
	// preserved rather than silently pointing at a different folder.
	dir := strings.Trim(string(out), "\r\n")
	if dir == "" {
		return "", false, nil
	}
	return dir, true, nil
}
