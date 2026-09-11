//go:build !darwin && !linux && !windows

package platform

import "errors"

func promptName(title, message, initial string) (string, bool, error) {
	return "", false, errors.New("native name prompt not implemented on this platform")
}
