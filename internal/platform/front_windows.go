//go:build windows

package platform

// frontType compiles a helper that makes a window the foreground window. A
// dialog's PowerShell is never the foreground process, so Windows refuses its
// SetForegroundWindow and keys keep going to the browser; attaching to the
// foreground thread's input first lifts that. A failed compile is ignored and
// the dialog still shows, on top but unfocused.
const frontType = `try { Add-Type -Namespace HtmlClayUI -Name Front -MemberDefinition '` +
	`[DllImport("user32.dll")] static extern IntPtr GetForegroundWindow(); ` +
	`[DllImport("user32.dll")] static extern uint GetWindowThreadProcessId(IntPtr h, IntPtr p); ` +
	`[DllImport("kernel32.dll")] static extern uint GetCurrentThreadId(); ` +
	`[DllImport("user32.dll")] static extern bool AttachThreadInput(uint a, uint b, bool attach); ` +
	`[DllImport("user32.dll")] static extern bool SetForegroundWindow(IntPtr h); ` +
	`public static void Window(IntPtr h) { ` +
	`uint fg = GetWindowThreadProcessId(GetForegroundWindow(), IntPtr.Zero); ` +
	`uint me = GetCurrentThreadId(); ` +
	`bool attached = fg != 0 && fg != me && AttachThreadInput(me, fg, true); ` +
	`SetForegroundWindow(h); ` +
	`if (attached) AttachThreadInput(me, fg, false); }' } catch { }; `

// frontOnShown brings the form $f to the foreground once it is on screen.
const frontOnShown = `$f.Add_Shown({ if ('HtmlClayUI.Front' -as [type]) { [HtmlClayUI.Front]::Window($f.Handle) }; $f.Activate() }); `
