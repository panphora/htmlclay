package tray

import (
	"testing"
)

func TestIconEmbedded(t *testing.T) {
	if len(iconBytes) == 0 {
		t.Fatal("icon.png not embedded")
	}
	if iconBytes[0] != 0x89 || iconBytes[1] != 'P' || iconBytes[2] != 'N' || iconBytes[3] != 'G' {
		t.Fatal("embedded icon is not a valid PNG")
	}
}
