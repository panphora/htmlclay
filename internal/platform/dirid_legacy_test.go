//go:build !windows

package platform

import (
	"testing"
)

func TestMatchPinRules(t *testing.T) {
	home := dirStat{dev: 16777232, ino: 2, vol: "HOMEVOL"}
	here := dirStat{dev: 16777232, ino: 381827741, vol: "HOMEVOL"}
	other := dirStat{dev: 16777240, ino: 381827741, vol: "OTHERVOL"}
	noVol := dirStat{dev: 16777232, ino: 381827741}
	cases := []struct {
		name   string
		pinned string
		s      dirStat
		want   bool
	}{
		{"current v2 pin", "v2:HOMEVOL:381827741", here, true},
		{"v2 pin, same inode on another volume", "v2:HOMEVOL:381827741", other, false},
		{"v2 pin, folder replaced", "v2:HOMEVOL:381827741", dirStat{dev: 16777232, ino: 99, vol: "HOMEVOL"}, false},
		{"legacy pin, dev renumbered at boot, home volume", "16777229:381827741", here, true},
		{"legacy pin, exact, home volume", "16777232:381827741", here, true},
		{"legacy pin, exact, off home's volume", "16777240:381827741", other, true},
		{"legacy pin, folder replaced", "16777229:99", here, false},
		{"legacy pin, same inode off home's volume", "16777229:381827741", other, false},
		{"legacy pin, filesystem without a volume id, dev moved", "16777229:381827741", noVol, false},
		{"legacy pin, filesystem without a volume id, exact", "16777232:381827741", noVol, true},
		{"garbage pin", "not-the-folder-on-disk", here, false},
		{"legacy-looking pin with junk", "16777229:381827741x", here, false},
	}
	for _, c := range cases {
		if got := matchPin(c.pinned, c.s, home); got != c.want {
			t.Errorf("%s: matchPin(%q) = %v, want %v", c.name, c.pinned, got, c.want)
		}
	}
}

func TestMatchPinNeedsAVolumeIDForAnInodeOnlyMatch(t *testing.T) {
	home := dirStat{dev: 16777232, ino: 2}
	s := dirStat{dev: 16777232, ino: 381827741}
	if matchPin("16777229:381827741", s, home) {
		t.Fatal("without a volume id an inode match alone must not vouch for a legacy pin")
	}
}
