package logging

import (
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

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				l.Printf("goroutine %d line %d", n, j)
			}
		}(i)
	}
	wg.Wait()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, line := range lines {
		if !strings.Contains(line, "goroutine") {
			t.Errorf("corrupted line: %q", line)
			break
		}
	}
}
