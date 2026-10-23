package tailwind

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExtractCandidatesMatchesTailwindHyperclay(t *testing.T) {
	cases := []struct {
		name string
		html string
		want []string
	}{
		{
			name: "whitespace and case",
			html: "<div class=\"p-4  md:grid\nprose\"><span CLASS = 'hidden p-4'></span></div>",
			want: []string{"p-4", "md:grid", "prose", "hidden"},
		},
		{
			name: "quotes inside values and empty class",
			html: "<a class=\"x 'y' z\" class=\"\">t</a><p class=\"ephemeral-selected:ring-2 [&>*]:mt-2 w-1/2\"></p>",
			want: []string{"x", "ephemeral-selected:ring-2", "[&>*]:mt-2", "w-1/2"},
		},
		{
			name: "unquoted and lookalike attributes",
			html: "<b clazz=\"no\" data-class=\"yes\" class=bare></b>",
			want: []string{"yes"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractCandidates([]byte(tc.html))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ExtractCandidates(%q) = %q, want %q", tc.html, got, tc.want)
			}
		})
	}
}

func TestCandidatesStartWithDefaults(t *testing.T) {
	want := append(append([]string{}, defaultCandidates...), "p-4")
	got := Candidates([]byte(`<i class="p-4">`))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Candidates = %q, want %q", got, want)
	}
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-tailwind")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCompileWritesInputsAndReadsOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	bin := writeScript(t, `if [ "$#" -ne 4 ] || [ "$1" != "-i" ] || [ "$2" != "in.css" ] || [ "$3" != "-o" ] || [ "$4" != "out.css" ]; then
  echo "unexpected args: $@" >&2
  exit 3
fi
cat in.css candidates.txt ephemeral.js > out.css
`)

	out, err := Compile(context.Background(), bin, []byte(`<p class="prose p-4">`))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	s := string(out)
	for _, want := range []string{`@source "./candidates.txt"`, "source(none)", "\np-4\n", "\nhidden\n", "matchVariant('ephemeral'"} {
		if !strings.Contains(s, want) {
			t.Fatalf("output does not contain %q:\n%s", want, s)
		}
	}
}

func TestCompileReportsFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	bin := writeScript(t, "echo boom >&2\nexit 1\n")

	_, err := Compile(context.Background(), bin, []byte(`<p class="p-4">`))
	if err == nil {
		t.Fatal("Compile returned no error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error %q does not contain %q", err, "boom")
	}
}

func TestCompileTimesOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	bin := writeScript(t, "exec sleep 5\n")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := Compile(ctx, bin, []byte(`<p class="p-4">`)); err == nil {
		t.Fatal("Compile returned no error")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("Compile took %s, want under 4s", elapsed)
	}
}

func TestCompileRealBinary(t *testing.T) {
	if os.Getenv("HTMLCLAY_TAILWIND_IT") != "1" {
		t.Skip("set HTMLCLAY_TAILWIND_IT=1 to run")
	}
	bin := os.Getenv("HTMLCLAY_TAILWIND_BIN")
	if bin == "" {
		t.Fatal("HTMLCLAY_TAILWIND_BIN is empty")
	}

	out, err := Compile(context.Background(), bin, []byte(`<div class="prose form-input ephemeral-selected:ring-2 p-4 md:grid">`))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	s := string(out)
	t.Logf("stylesheet is %d bytes", len(out))
	for _, want := range []string{".prose", ".form-input", `[ephemeral~="selected"]`, ".p-4", `md\:grid`, ".hidden"} {
		if !strings.Contains(s, want) {
			t.Fatalf("output does not contain %q", want)
		}
	}
}
