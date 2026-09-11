//go:build !darwin && !linux && !windows

package platform

import "errors"

func selectFile(prompt string) (string, bool, error) {
	return "", false, errors.New("native file picker not implemented on this platform")
}
