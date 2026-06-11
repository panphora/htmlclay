package tray

import (
	"testing"
)

func TestIconEmbedded(t *testing.T) {
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"icon.png", iconBytes},
		{"icon-template.png", iconTemplateBytes},
	} {
		if len(c.data) == 0 {
			t.Fatalf("%s not embedded", c.name)
		}
		if c.data[0] != 0x89 || c.data[1] != 'P' || c.data[2] != 'N' || c.data[3] != 'G' {
			t.Fatalf("embedded %s is not a valid PNG", c.name)
		}
	}
}
