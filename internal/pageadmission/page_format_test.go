package pageadmission

import (
	"errors"
	"strings"
	"testing"
)

const formatDocHead = `<!DOCTYPE html><html><head></head><body><p>text</p></body></html>`
const formatDocNoHead = `<!DOCTYPE html><html><body><p>text</p></body></html>`

func TestPageFormatFloorReadsTheMarker(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want int
	}{
		{"absent", formatDocHead, 0},
		{"double quoted", `<html><head><meta name="clay-page-format" content="1"></head></html>`, 1},
		{"single quoted", `<html><head><meta name='clay-page-format' content='1'></head></html>`, 1},
		{"unquoted", `<html><head><meta name=clay-page-format content=1></head></html>`, 1},
		{"uppercase name", `<html><head><meta NAME="clay-page-format" CONTENT="2"></head></html>`, 2},
		{"attributes in either order", `<html><head><meta content="3" name="clay-page-format"></head></html>`, 3},
		{"unreadable content", `<html><head><meta name="clay-page-format" content="x"></head></html>`, 0},
		{"negative content", `<html><head><meta name="clay-page-format" content="-1"></head></html>`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PageFormatFloor(tc.doc); got != tc.want {
				t.Fatalf("PageFormatFloor(%q) = %d, want %d", tc.doc, got, tc.want)
			}
		})
	}
}

func TestPageFormatWithFloorIsIdempotent(t *testing.T) {
	marked := WithPageFormatFloor(formatDocHead, 1)
	if want := `<html><head><meta name="clay-page-format" content="1"></head>`; !strings.Contains(marked, want) {
		t.Fatalf("marker was not placed before </head>: %q", marked)
	}
	if again := WithPageFormatFloor(marked, 1); again != marked {
		t.Fatalf("applying the floor twice changed the bytes:\n%q\n%q", marked, again)
	}
	if got := WithPageFormatFloor(formatDocHead, 0); got != formatDocHead {
		t.Fatalf("floor 0 changed the document: %q", got)
	}

	noHead := WithPageFormatFloor(formatDocNoHead, 1)
	if want := `<meta name="clay-page-format" content="1"><body>`; !strings.Contains(noHead, want) {
		t.Fatalf("marker was not placed before <body>: %q", noHead)
	}

	raised := WithPageFormatFloor(formatDocHead, 2)
	if !strings.Contains(raised, `content="2"`) {
		t.Fatalf("a higher floor did not replace the marker: %q", raised)
	}
}

func TestPageFormatCheck(t *testing.T) {
	floorZero := formatDocHead
	floorOne := WithPageFormatFloor(formatDocHead, 1)
	declaresOne := WithPageFormatFloor(formatDocHead, 1)

	cases := []struct {
		name       string
		current    string
		next       string
		capability int
		ok         bool
		stored     string
		minimum    int
	}{
		{"floor 0 with capability 0", floorZero, floorZero, 0, true, floorZero, 0},
		{"floor 0 declaring 1 with capability 1", floorZero, declaresOne, 1, true, declaresOne, 0},
		{"floor 0 declaring 1 with capability 0", floorZero, declaresOne, 0, false, "", 1},
		{"floor 1 omitting the marker with capability 1", floorOne, floorZero, 1, true, declaresOne, 0},
		{"floor 1 with capability 0", floorOne, floorZero, 0, false, "", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored, err := CheckPageFormat(tc.current, tc.next, tc.capability)
			if tc.ok {
				if err != nil {
					t.Fatalf("refused an admissible write: %v", err)
				}
				if stored != tc.stored {
					t.Fatalf("stored = %q, want %q", stored, tc.stored)
				}
				return
			}
			var refusal *FormatRefusal
			if !errors.As(err, &refusal) || refusal.Minimum != tc.minimum {
				t.Fatalf("err = %v, want a refusal with minimum %d", err, tc.minimum)
			}
			if stored != "" {
				t.Fatalf("a refused write returned bytes: %q", stored)
			}
		})
	}
}

func TestPageFormatCapabilityHeaderValues(t *testing.T) {
	for _, value := range []string{"", "abc", "1.5"} {
		if got := FormatCapability(value); got != 0 {
			t.Errorf("FormatCapability(%q) = %d, want 0", value, got)
		}
	}
	if got := FormatCapability(" 1 "); got != 1 {
		t.Errorf("FormatCapability(%q) = %d, want 1", " 1 ", got)
	}
}
