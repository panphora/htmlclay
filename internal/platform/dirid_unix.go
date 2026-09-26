//go:build !windows

package platform

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// dirStat is what a directory fingerprint is built from. vol is a volume id
// that survives a reboot, or "" where the platform has none; dev does not
// survive one on macOS, which renumbers APFS volumes at mount.
type dirStat struct {
	dev, ino uint64
	vol      string
}

// statPath stats path, following symlinks, and reports no volume id.
func statPath(path string) (dirStat, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return dirStat{}, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return dirStat{}, false
	}
	return dirStat{dev: uint64(st.Dev), ino: uint64(st.Ino)}, true
}

// identity is "v2:<volume>:<inode>" where a stable volume id exists, else the
// original "<dev>:<inode>". The prefix keeps the two forms from ever comparing
// equal.
func (s dirStat) identity() string {
	if s.vol == "" {
		return fmt.Sprintf("%d:%d", s.dev, s.ino)
	}
	return fmt.Sprintf("v2:%s:%d", s.vol, s.ino)
}

// DirIdentity returns a volume+inode fingerprint for the directory at path, or
// "" when one cannot be derived.
func DirIdentity(path string) string {
	s, ok := statDir(path)
	if !ok {
		return ""
	}
	return s.identity()
}

// MatchDirIdentity reports whether the directory at path is the one pinned, and
// returns the fingerprint it has now, which is what the pin should become.
//
// A pin in the old "<dev>:<inode>" form is accepted when both still match, and
// otherwise on inode alone only when the directory sits on the same volume as
// home. macOS assigns st_dev at mount, so every such pin went stale at the first
// reboot after it was taken. Inode numbers are unique within a volume, so on
// home's volume an inode match is the same directory, and a swap for a symlink
// or a recreated folder still fails it. Off home's volume nothing can vouch for
// the old pin, and the folder stays dead until the user approves it again.
func MatchDirIdentity(path, pinned, home string) (current string, ok bool) {
	s, found := statDir(path)
	if !found {
		return "", false
	}
	h, _ := statDir(home)
	return s.identity(), matchPin(pinned, s, h)
}

func matchPin(pinned string, s, home dirStat) bool {
	if pinned == s.identity() {
		return true
	}
	dev, ino, legacy := parseLegacyPin(pinned)
	if !legacy || ino != s.ino {
		return false
	}
	if dev == s.dev {
		return true
	}
	return s.vol != "" && s.vol == home.vol
}

func parseLegacyPin(pinned string) (dev, ino uint64, ok bool) {
	d, i, found := strings.Cut(pinned, ":")
	if !found {
		return 0, 0, false
	}
	dev, err := strconv.ParseUint(d, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	ino, err = strconv.ParseUint(i, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return dev, ino, true
}
