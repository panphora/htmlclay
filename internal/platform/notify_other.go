//go:build !darwin && !linux && !windows

package platform

func notify(title, message string) error { return nil }

func inform(title, message string) error { return nil }
