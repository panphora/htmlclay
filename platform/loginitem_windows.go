//go:build windows

package platform

import "os/exec"

func SetLoginItem(enabled bool, executablePath string) error {
	if enabled {
		return exec.Command("reg", "add",
			`HKCU\Software\Microsoft\Windows\CurrentVersion\Run`,
			"/v", "HTMLClay",
			"/d", `"`+executablePath+`"`,
			"/f").Run()
	}
	return exec.Command("reg", "delete",
		`HKCU\Software\Microsoft\Windows\CurrentVersion\Run`,
		"/v", "HTMLClay",
		"/f").Run()
}

func IsLoginItem() bool {
	err := exec.Command("reg", "query",
		`HKCU\Software\Microsoft\Windows\CurrentVersion\Run`,
		"/v", "HTMLClay").Run()
	return err == nil
}
