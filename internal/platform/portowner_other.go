//go:build !darwin && !linux && !windows

package platform

func portOwner(int) (int, string, bool) { return 0, "", false }
