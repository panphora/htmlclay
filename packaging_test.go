package main

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// These tests pin couplings that live in two files at once and break silently.
// Nothing in a build or a unit test notices a document icon that stopped being
// found: the app runs, the file opens, and the only symptom is a blank page icon
// on a machine none of the developers is using.

// On Windows the DefaultIcon registry value names the document icon by RESOURCE
// ID rather than by position, because the position depends on link order and the
// ID does not. That only works while the ID in platform/register_windows.go and
// the ID in winres.json are the same number, and they are written in different
// languages three directories apart.
func TestWindowsDocumentIconIDMatchesTheRegistryValue(t *testing.T) {
	// type -> resource name -> language -> the resource itself, which is a
	// filename for an icon group and an object for version info.
	var res map[string]map[string]map[string]json.RawMessage
	data, err := os.ReadFile(filepath.Join("dist", "windows", "winres.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatalf("winres.json is not valid JSON: %v", err)
	}

	icons := res["RT_GROUP_ICON"]
	if len(icons) != 2 {
		t.Fatalf("expected exactly two icon groups (the app icon and the document icon), got %d", len(icons))
	}
	for id, want := range map[string]string{"#1": "htmlclay.ico", "#2": "doc.ico"} {
		langs, ok := icons[id]
		if !ok {
			t.Fatalf("winres.json declares no icon group %s", id)
		}
		var file string
		for _, raw := range langs {
			if err := json.Unmarshal(raw, &file); err != nil {
				t.Fatalf("icon group %s does not name a single file: %v", id, err)
			}
		}
		if file != want {
			t.Errorf("icon group %s is built from %q, want %q", id, file, want)
		}
		if _, err := os.Stat(filepath.Join("dist", "windows", file)); err != nil {
			t.Errorf("icon group %s names %s, which is not there: %v", id, file, err)
		}
	}

	// Read the constant out of the source rather than importing it: it is behind
	// a windows build tag, and this test has to fail on the machine of whoever
	// renumbers the icon, whatever they are running.
	src, err := os.ReadFile(filepath.Join("platform", "register_windows.go"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`docIconIndex = "(,-\d+)"`).FindSubmatch(src)
	if m == nil {
		t.Fatal("platform/register_windows.go no longer declares docIconIndex as a negative icon index")
	}
	if got := string(m[1]); got != ",-2" {
		t.Errorf("DefaultIcon points at %q, but the document icon is resource 2 in winres.json", got)
	}
}

// On Linux the document icon is found by filename alone: freedesktop looks up a
// MIME type's icon under mimetypes/ as the type with its slash turned into a
// dash. Rename the type or the file and the icon quietly stops being found.
func TestLinuxDocumentIconIsNamedAfterTheMimeType(t *testing.T) {
	var info struct {
		Types []struct {
			Type string `xml:"type,attr"`
		} `xml:"mime-type"`
	}
	data, err := os.ReadFile(filepath.Join("dist", "linux", "htmlclay-mime.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := xml.Unmarshal(data, &info); err != nil {
		t.Fatalf("htmlclay-mime.xml is not valid XML: %v", err)
	}
	if len(info.Types) != 1 {
		t.Fatalf("expected one declared MIME type, got %d", len(info.Types))
	}

	iconName := strings.ReplaceAll(info.Types[0].Type, "/", "-")
	installer, err := os.ReadFile(filepath.Join("dist", "linux", "install-icon.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{".png", ".svg"} {
		file := iconName + ext
		if _, err := os.Stat(filepath.Join("dist", "linux", file)); err != nil {
			t.Errorf("the MIME type is %s, so its icon must be shipped as %s: %v", info.Types[0].Type, file, err)
		}
		if !strings.Contains(string(installer), file) {
			t.Errorf("install-icon.sh does not install %s, so the icon would never reach the icon theme", file)
		}
	}
}
