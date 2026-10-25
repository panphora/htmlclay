package pageadmission

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/panphora/htmlclay/internal/specwire"
)

type lineageVectors struct {
	V      int            `json:"v"`
	Writes []lineageWrite `json:"writes"`
}

type lineageWrite struct {
	Name      string  `json:"name"`
	Current   string  `json:"current"`
	Body      string  `json:"body"`
	IfMatchOf *string `json:"ifMatchOf"`
	PageWrite *string `json:"pageWrite"`
	Expect    struct {
		JS       string `json:"js"`
		HTMLClay string `json:"htmlclay"`
	} `json:"expect"`
}

// goResult applies the save handler's order to one vector.
func (w lineageWrite) goResult(t *testing.T, seedStamp string) string {
	t.Helper()
	stamp, sent := "", false
	if w.IfMatchOf != nil {
		sent = true
		switch *w.IfMatchOf {
		case "current":
			stamp = specwire.Etag([]byte(w.Current))
		case "T":
			stamp = seedStamp
		default:
			t.Fatalf("%s: unknown ifMatchOf %q", w.Name, *w.IfMatchOf)
		}
	}
	if sent && stamp != specwire.Etag([]byte(w.Current)) {
		return "412"
	}
	decision, err := CheckWrite(w.Current, w.Body, "")
	if err != nil {
		var refused *Refusal
		if !errors.As(err, &refused) {
			return "error: " + err.Error()
		}
		return refused.Reason
	}
	if !sent && decision != Ordinary {
		return "file-changed"
	}
	if decision == Ordinary && w.PageWrite != nil && *w.PageWrite == "v1" && NothingToWrite(w.Current, w.Body) {
		return "nothing-to-write"
	}
	if decision == Initial {
		return "initial"
	}
	return "ordinary"
}

func TestPageLineageVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/page-lineage.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors lineageVectors
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("parsing vectors: %v", err)
	}
	if len(vectors.Writes) == 0 {
		t.Fatal("the vector file carries no writes[]")
	}
	seedStamp := ""
	for _, w := range vectors.Writes {
		if w.Name == "seed-only-v1" {
			seedStamp = specwire.Etag([]byte(w.Current))
		}
	}
	if seedStamp == "" {
		t.Fatal("the vector file carries no seed-only-v1 entry")
	}
	for _, w := range vectors.Writes {
		t.Run(w.Name, func(t *testing.T) {
			got := w.goResult(t, seedStamp)
			if got != w.Expect.HTMLClay {
				t.Fatalf("htmlclay = %q, want %q", got, w.Expect.HTMLClay)
			}
			if w.Expect.JS == "seed-lineage" && got == "ordinary" &&
				Inspect(StripPageTag(w.Body)).Kind != "none" {
				t.Fatalf("stale lineage would reach disk: %q", Inspect(StripPageTag(w.Body)).Kind)
			}
		})
	}
	t.Logf("%d page-lineage vectors", len(vectors.Writes))
}

func TestNothingToWrite(t *testing.T) {
	raw, err := os.ReadFile("testdata/page-lineage.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors lineageVectors
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("parsing vectors: %v", err)
	}
	byName := map[string]lineageWrite{}
	for _, w := range vectors.Writes {
		byName[w.Name] = w
	}
	for _, test := range []struct {
		name  string
		want  bool
		write lineageWrite
	}{
		{"clean enter and exit", true, byName["seed-only-v1"]},
		{"empty page's clean enter and exit", true, byName["empty-seed-only-v1"]},
		{"long page's clean enter and exit", true, byName["large-seed-only-v1"]},
		{"first edit", false, byName["first-edit"]},
		{"delete only", false, byName["delete-only"]},
		{"new site from template", false, byName["new-site-from-template"]},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := NothingToWrite(test.write.Current, test.write.Body); got != test.want {
				t.Fatalf("NothingToWrite = %v, want %v", got, test.want)
			}
		})
	}
}
