package update

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

const keptHandoff = `{"schema":1,"transaction":"abc","created":"2026-10-14T20:00:00Z","targetVersion":"2.0.0","port":4123,"sites":[{"key":"/k","slug":"htmlclay","files":[{"path":"/k/a.htmlclay","via":"trusted","token":"tok"}]}],"futureField":true}`

func TestRetargetHandoffPointsItAtTheOldVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-handoff.json")
	before := time.Now().UTC().Add(-time.Second)
	if err := retargetHandoff(path, []byte(keptHandoff), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	assertRetargeted(t, path, "1.0.0", before)
}

func TestRetargetHandoffWithNothingKeptWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-handoff.json")
	if err := retargetHandoff(path, nil, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := retargetHandoff("", []byte(keptHandoff), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a handoff appeared from nothing")
	}
}

// assertRetargeted checks the handoff at path now targets version, was created
// after before, carries every other field unchanged, and is private.
func assertRetargeted(t *testing.T, path, version string, before time.Time) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the handoff is gone after the rollback: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	var target string
	var created time.Time
	json.Unmarshal(got["targetVersion"], &target)
	json.Unmarshal(got["created"], &created)
	if target != version {
		t.Fatalf("targetVersion %q, want %q", target, version)
	}
	if created.Before(before) {
		t.Fatalf("created %s was not refreshed (want after %s)", created, before)
	}
	var orig map[string]json.RawMessage
	json.Unmarshal([]byte(keptHandoff), &orig)
	for k, v := range orig {
		if k == "targetVersion" || k == "created" {
			continue
		}
		var compact bytes.Buffer
		json.Compact(&compact, got[k])
		if compact.String() != string(v) {
			t.Fatalf("field %s changed: %s, want %s", k, compact.String(), v)
		}
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("handoff mode %v, want 0600", info.Mode().Perm())
		}
	}
}
