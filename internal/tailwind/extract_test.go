package tailwind

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var testPayload = []byte("#!/bin/sh\necho ok\n")

// testBlob gzips payload and reports the uncompressed checksum and size, the
// way the fetch tool writes them.
func testBlob(t *testing.T, payload []byte) (gz []byte, sum string, size int64) {
	t.Helper()
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(payload)
	return buf.Bytes(), hex.EncodeToString(s[:]), int64(len(payload))
}

func installedPath(root, sum string) string {
	return filepath.Join(root, Version+"-"+sum[:12], exeName())
}

// resetVerified clears the package memo so each test sees a fresh install.
func resetVerified(t *testing.T) {
	t.Helper()
	verified = ""
	t.Cleanup(func() { verified = "" })
}

func TestInstallExtractsAndVerifies(t *testing.T) {
	resetVerified(t)
	root := t.TempDir()
	gz, sum, size := testBlob(t, testPayload)

	path, err := install(root, gz, sum, size)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if want := installedPath(root, sum); path != want {
		t.Fatalf("install returned %q, want %q", path, want)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading extracted binary: %v", err)
	}
	if !bytes.Equal(got, testPayload) {
		t.Fatalf("extracted %q, want %q", got, testPayload)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o100 == 0 {
			t.Fatalf("extracted file mode %v has no owner-execute bit", info.Mode().Perm())
		}
	}
}

func TestInstallReusesVerifiedFile(t *testing.T) {
	root := t.TempDir()
	gz, sum, size := testBlob(t, testPayload)

	verified = ""
	t.Cleanup(func() { verified = "" })
	first, err := install(root, gz, sum, size)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	// A rewrite would carry a later time, so an earlier one proves reuse.
	before := info.ModTime().Add(-time.Hour)
	if err := os.Chtimes(first, before, before); err != nil {
		t.Fatal(err)
	}

	verified = ""
	second, err := install(root, gz, sum, size)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if second != first {
		t.Fatalf("second install returned %q, want %q", second, first)
	}
	info, err = os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(before) {
		t.Fatalf("second install rewrote the file: ModTime %v, want %v", info.ModTime(), before)
	}
}

func TestInstallReplacesTamperedFile(t *testing.T) {
	root := t.TempDir()
	gz, sum, size := testBlob(t, testPayload)

	verified = ""
	t.Cleanup(func() { verified = "" })
	path, err := install(root, gz, sum, size)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := os.WriteFile(path, []byte("tampered\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	verified = ""
	again, err := install(root, gz, sum, size)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	got, err := os.ReadFile(again)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, testPayload) {
		t.Fatalf("extracted %q, want %q", got, testPayload)
	}
}

func TestInstallRecoversDeletedBinary(t *testing.T) {
	root := t.TempDir()
	gz, sum, size := testBlob(t, testPayload)

	verified = ""
	t.Cleanup(func() { verified = "" })
	path, err := install(root, gz, sum, size)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	again, err := install(root, gz, sum, size)
	if err != nil {
		t.Fatalf("install after the binary was deleted: %v", err)
	}
	got, err := os.ReadFile(again)
	if err != nil {
		t.Fatalf("reading the reinstalled binary: %v", err)
	}
	if !bytes.Equal(got, testPayload) {
		t.Fatalf("extracted %q, want %q", got, testPayload)
	}
}

// A file whose bytes match but whose mode does not is not the compiler: the
// kernel refuses to run it, so it is replaced rather than reused.
func TestInstallRepairsNonExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not meaningful on Windows")
	}
	resetVerified(t)
	root := t.TempDir()
	gz, sum, size := testBlob(t, testPayload)

	path, err := install(root, gz, sum, size)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	verified = ""
	if _, err := install(root, gz, sum, size); err != nil {
		t.Fatalf("install over a non-executable file: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("mode %v still has no owner-execute bit", info.Mode().Perm())
	}
}

func TestInstallRemovesStaleTemps(t *testing.T) {
	resetVerified(t)
	root := t.TempDir()
	gz, sum, size := testBlob(t, testPayload)

	dir := filepath.Join(root, Version+"-"+sum[:12])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, ".tailwindcss-old")
	fresh := filepath.Join(dir, ".tailwindcss-new")
	if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fresh, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	hourAgo := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, hourAgo, hourAgo); err != nil {
		t.Fatal(err)
	}

	if _, err := install(root, gz, sum, size); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("stale temp survived the install: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a fresh temp was removed: %v", err)
	}
}

func TestInstallRejectsWrongChecksum(t *testing.T) {
	resetVerified(t)
	root := t.TempDir()
	gz, _, size := testBlob(t, testPayload)
	wrong := string(bytes.Repeat([]byte("0"), 64))

	if _, err := install(root, gz, wrong, size); err == nil {
		t.Fatal("install with a wrong checksum succeeded")
	}
	if _, err := os.Stat(installedPath(root, wrong)); !os.IsNotExist(err) {
		t.Fatalf("a wrong-checksum install left a file behind: %v", err)
	}
}

func TestInstallPrunesOtherVersions(t *testing.T) {
	resetVerified(t)
	root := t.TempDir()
	gz, sum, size := testBlob(t, testPayload)

	stale := filepath.Join(root, "4.0.0-aaaaaaaaaaaa")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "x"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	path, err := install(root, gz, sum, size)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale version folder survived: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("installed file is missing: %v", err)
	}
}

func TestLocateWithoutBundle(t *testing.T) {
	if Bundled() {
		t.Skip("bundled build")
	}
	resetVerified(t)
	root := t.TempDir()

	t.Setenv("HTMLCLAY_TAILWIND_BIN", "")
	if _, err := Locate(root); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Locate with no bundle and no env = %v, want ErrUnavailable", err)
	}

	t.Setenv("HTMLCLAY_TAILWIND_BIN", "/x/tw")
	path, err := Locate(root)
	if err != nil {
		t.Fatalf("Locate with HTMLCLAY_TAILWIND_BIN = %v", err)
	}
	if path != "/x/tw" {
		t.Fatalf("Locate = %q, want /x/tw", path)
	}
}

func TestBundledBinaryMatchesAsset(t *testing.T) {
	if !Bundled() {
		t.Skip("untagged build")
	}
	resetVerified(t)
	sum, size, err := parseSum(bundledSum)
	if err != nil {
		t.Fatalf("parseSum: %v", err)
	}
	if size <= 0 {
		t.Fatalf("bundled size = %d", size)
	}

	path, err := install(t.TempDir(), bundled, sum, size)
	if err != nil {
		t.Fatalf("install bundled binary: %v", err)
	}
	cmd := exec.Command(path, "--help")
	cmd.WaitDelay = 5 * time.Second
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", path, err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s --help: %v", path, err)
		}
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("%s --help did not finish in 60s", path)
	}

	out, err := Compile(context.Background(), path, []byte(`<div class="prose form-input ephemeral-selected:ring-2 p-4 md:grid">`))
	if err != nil {
		t.Fatalf("Compile with the bundled binary: %v", err)
	}
	for _, want := range []string{".prose", ".form-input", `[ephemeral~="selected"]`, ".p-4", `md\:grid`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("compiled stylesheet does not contain %q:\n%s", want, out)
		}
	}
}
