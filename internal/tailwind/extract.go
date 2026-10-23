package tailwind

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	installMu sync.Mutex
	verified  string
)

func exeName() string {
	if runtime.GOOS == "windows" {
		return "tailwindcss.exe"
	}
	return "tailwindcss"
}

func parseSum(s string) (sum string, size int64, err error) {
	f := strings.Fields(s)
	if len(f) != 2 || len(f[0]) != 64 {
		return "", 0, errors.New("tailwind: malformed bundled checksum")
	}
	size, err = strconv.ParseInt(f[1], 10, 64)
	if err != nil || size <= 0 {
		return "", 0, errors.New("tailwind: malformed bundled size")
	}
	return f[0], size, nil
}

// install puts the binary gz decompresses to under root, in a folder named for
// the version and checksum, and returns its path. A file already there is
// reused only once its hash matches. Other version folders are removed.
func install(root string, gz []byte, sum string, size int64) (string, error) {
	installMu.Lock()
	defer installMu.Unlock()

	dir := filepath.Join(root, Version+"-"+sum[:12])
	path := filepath.Join(dir, exeName())
	if verified == path {
		if info, err := os.Stat(path); err == nil && info.Size() == size &&
			(runtime.GOOS == "windows" || info.Mode().Perm()&0o100 != 0) {
			return path, nil
		}
		verified = ""
	}
	if fileMatches(path, sum, size) {
		verified = path
		prune(root, dir)
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	removeStaleTemps(dir, time.Now().Add(-10*time.Minute))
	tmp, err := os.CreateTemp(dir, ".tailwindcss-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		tmp.Close()
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(zr, size+1))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if n != size || hex.EncodeToString(h.Sum(nil)) != sum {
		return "", fmt.Errorf("tailwind: bundled binary does not match its checksum")
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	if err := renameRetry(tmp.Name(), path); err != nil {
		if fileMatches(path, sum, size) {
			verified = path
			prune(root, dir)
			return path, nil
		}
		return "", err
	}
	verified = path
	prune(root, dir)
	return path, nil
}

func fileMatches(path, sum string, size int64) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() != size {
		return false
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 {
		return false
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == sum
}

// Windows refuses a rename while a scanner holds the file; retry briefly.
func renameRetry(from, to string) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = os.Rename(from, to); err == nil || runtime.GOOS != "windows" {
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
	return err
}

// removeStaleTemps deletes extraction temp files an interrupted run left behind.
// A fresh one may belong to another HTML Clay extracting right now, so only old
// ones go.
func removeStaleTemps(dir string, before time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".tailwindcss-") {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(before) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func prune(root, keep string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if e.IsDir() && p != keep {
			os.RemoveAll(p)
		}
	}
}
