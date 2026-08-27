//go:build windows

package platform

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

const loginItemKey = `Software\Microsoft\Windows\CurrentVersion\Run`

func SetLoginItem(enabled bool, executablePath string) error {
	if enabled {
		key, _, err := registry.CreateKey(registry.CURRENT_USER, loginItemKey, registry.SET_VALUE)
		if err != nil {
			return err
		}
		defer key.Close()
		return key.SetStringValue("HTMLClay", `"`+executablePath+`"`)
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, loginItemKey, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer key.Close()
	if err := key.DeleteValue("HTMLClay"); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

func IsLoginItem() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, loginItemKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	_, _, err = key.GetStringValue("HTMLClay")
	return err == nil
}
