package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLogFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	l, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	l.Printf("hello %s", "world")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	line := string(data)
	if !strings.Contains(line, "hello world") {
		t.Errorf("log missing message: %q", line)
	}
	if !strings.Contains(line, "T") || !strings.Contains(line, "Z") {
		t.Errorf("log missing ISO timestamp: %q", line)
	}
}

func TestLogRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	l, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	bigLine := strings.Repeat("x", 1024)
	for i := 0; i < 11000; i++ {
		l.Printf("%s", bigLine)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maxLogSize {
		t.Errorf("log file too large after rotation: %d bytes", info.Size())
	}

	rotated := path + ".1"
	if _, err := os.Stat(rotated); os.IsNotExist(err) {
		t.Error("rotated log file does not exist")
	}
}

func TestConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	l, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	const goroutines, perGoroutine = 100, 100
	// A start barrier, so the writers genuinely overlap rather than being spread
	// out by however long it takes to spawn a hundred goroutines.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			for j := 0; j < perGoroutine; j++ {
				l.Printf("goroutine %d line %d", n, j)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Every line must arrive, not just every line that arrived must be intact.
	// Checking only the surviving lines lets a lock that drops writes pass: the
	// remainder are all well-formed, so nothing complains. 10,000 lines is well
	// under the 10MB rotation threshold, so they are all in this one file.
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != goroutines*perGoroutine {
		t.Fatalf("log holds %d lines, want %d: writes were lost", len(lines), goroutines*perGoroutine)
	}
	seen := make(map[string]bool, goroutines*perGoroutine)
	for _, line := range lines {
		i := strings.Index(line, "goroutine ")
		if i < 0 {
			t.Fatalf("corrupted line: %q", line)
		}
		if !seen[line[i:]] {
			seen[line[i:]] = true
			continue
		}
		t.Fatalf("duplicated line: %q", line)
	}
	for n := 0; n < goroutines; n++ {
		for j := 0; j < perGoroutine; j++ {
			if want := fmt.Sprintf("goroutine %d line %d", n, j); !seen[want] {
				t.Fatalf("missing %q", want)
			}
		}
	}
}
