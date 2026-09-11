package platform

import "time"

const selectFileTimeout = 5 * time.Minute

// SelectFile shows a native file picker. Cancel is ok=false with no error,
// matching SelectFolder.
func SelectFile(prompt string) (path string, ok bool, err error) {
	return selectFile(prompt)
}
