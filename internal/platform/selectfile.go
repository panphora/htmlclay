package platform

import "time"

const selectFileTimeout = 5 * time.Minute

// SelectFile shows a native file picker. dir is where the picker starts; empty
// leaves it to the OS. Cancel is ok=false with no error, matching SelectFolder.
func SelectFile(prompt, dir string) (path string, ok bool, err error) {
	return selectFile(prompt, dir)
}
