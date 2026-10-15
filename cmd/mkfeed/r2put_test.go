package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeAWS stands in for the two aws calls r2-put-immutable.sh makes, with a
// folder as the bucket: an object at <root>/<key> and its sha256 metadata in
// <root>/<key>.sha256. Every call is appended to <root>/calls.
const fakeAWS = `#!/usr/bin/env bash
set -euo pipefail
root=$FAKE_R2
echo "$*" >> "$root/calls"
if [ "$1 $2" = "s3api head-object" ]; then
  shift 2
  while [ $# -gt 0 ]; do case "$1" in --key) key=$2; shift 2 ;; *) shift ;; esac; done
  if [ -n "${FAKE_R2_HEAD_ERROR:-}" ]; then echo "$FAKE_R2_HEAD_ERROR" >&2; exit 255; fi
  if [ ! -f "$root/obj/$key" ]; then
    echo "An error occurred (404) when calling the HeadObject operation: Not Found" >&2
    exit 254
  fi
  if [ -f "$root/obj/$key.sha256" ]; then cat "$root/obj/$key.sha256"; else echo None; fi
  exit 0
fi
if [ "$1 $2" = "s3 cp" ]; then
  src=$3 dst=$4; shift 4; meta=""
  while [ $# -gt 0 ]; do case "$1" in --metadata) meta=${2#sha256=}; shift 2 ;; *) shift ;; esac; done
  case "$src" in
    s3://*) k=${src#s3://*/}; cp "$root/obj/$k" "$dst" ;;
    *) k=${dst#s3://*/}; mkdir -p "$(dirname "$root/obj/$k")"; cp "$src" "$root/obj/$k"
       if [ -n "$meta" ]; then echo "$meta" > "$root/obj/$k.sha256"; fi ;;
  esac
  exit 0
fi
echo "fake aws: unexpected call $*" >&2
exit 2
`

type r2Fixture struct {
	root, script, bin string
}

func newR2Fixture(t *testing.T) r2Fixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the release script runs on the Linux release runner")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(filepath.Join(root, "obj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "aws"), []byte(fakeAWS), 0o755); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "r2-put-immutable.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return r2Fixture{root: root, script: script, bin: bin}
}

func (f r2Fixture) put(t *testing.T, content, key string, env ...string) (string, error) {
	t.Helper()
	file := filepath.Join(t.TempDir(), filepath.Base(key))
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", f.script, file, key, "application/octet-stream")
	cmd.Env = append(os.Environ(), "PATH="+f.bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_R2="+f.root, "R2_BUCKET=bucket", "ENDPOINT=https://r2.invalid")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (f r2Fixture) object(t *testing.T, key string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.root, "obj", key))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (f r2Fixture) uploads(t *testing.T) int {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(f.root, "calls"))
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "s3 cp ") && !strings.HasPrefix(line, "s3 cp s3://") {
			n++
		}
	}
	return n
}

// A versioned update key is written once. Rerunning with the same bytes skips
// the upload; different bytes fail the release and leave the published object
// as it was, because a published feed may name it.
func TestR2PutImmutableNeverReplacesAVersionedObject(t *testing.T) {
	f := newR2Fixture(t)
	const key = "update/1.2.3/htmlclay-linux-amd64.gz"
	if out, err := f.put(t, "first build", key); err != nil {
		t.Fatalf("first upload: %v\n%s", err, out)
	}
	if f.object(t, key) != "first build" || f.uploads(t) != 1 {
		t.Fatalf("first upload stored %q with %d uploads", f.object(t, key), f.uploads(t))
	}

	out, err := f.put(t, "first build", key)
	if err != nil || !strings.Contains(out, "already holds these bytes") || f.uploads(t) != 1 {
		t.Fatalf("identical rerun: %v, %d uploads\n%s", err, f.uploads(t), out)
	}

	out, err = f.put(t, "rebuilt with other bytes", key)
	if err == nil || !strings.Contains(out, "needs a new version number") {
		t.Fatalf("a rebuild with different bytes was not refused: %v\n%s", err, out)
	}
	if f.object(t, key) != "first build" || f.uploads(t) != 1 {
		t.Fatalf("the published object was replaced: %q, %d uploads", f.object(t, key), f.uploads(t))
	}
}

// An object uploaded before the metadata existed is compared by its bytes.
func TestR2PutImmutableHashesAnObjectWithoutMetadata(t *testing.T) {
	f := newR2Fixture(t)
	const key = "update/1.2.3/HTMLClay-1.2.3-universal.app.zip"
	path := filepath.Join(f.root, "obj", key)
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte("legacy bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := f.put(t, "legacy bytes", key); err != nil || f.uploads(t) != 0 {
		t.Fatalf("identical legacy object: %v, %d uploads\n%s", err, f.uploads(t), out)
	}
	if out, err := f.put(t, "other bytes", key); err == nil || f.uploads(t) != 0 {
		t.Fatalf("a different legacy object was replaced: %v\n%s", err, out)
	}
}

// Anything but a clean "not found" from the existence check stops the
// release rather than uploading blind.
func TestR2PutImmutableStopsWhenTheCheckFails(t *testing.T) {
	f := newR2Fixture(t)
	out, err := f.put(t, "bytes", "update/1.2.3/x.gz", "FAKE_R2_HEAD_ERROR=An error occurred (403) when calling the HeadObject operation: Forbidden")
	if err == nil || f.uploads(t) != 0 {
		t.Fatalf("uploaded after a failed check: %v\n%s", err, out)
	}
}
