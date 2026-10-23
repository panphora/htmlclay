// Command fetchtailwind downloads the Tailwind CLI this platform's bundled
// build embeds, and writes the blob the tagged build compiles in.
package main

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/panphora/htmlclay/internal/tailwind"
)

func main() {
	goos := flag.String("goos", runtime.GOOS, "target GOOS")
	goarch := flag.String("goarch", runtime.GOARCH, "target GOARCH")
	file := flag.String("file", "", "read the binary from this file instead of downloading it; its payload is not checked against the upstream checksum")
	flag.Parse()

	if err := run(*goos, *goarch, *file); err != nil {
		fmt.Fprintln(os.Stderr, "fetchtailwind:", err)
		os.Exit(1)
	}
}

func run(goos, goarch, file string) error {
	asset, ok := tailwind.Assets[goos+"/"+goarch]
	if !ok {
		return fmt.Errorf("no Tailwind asset for %s/%s", goos, goarch)
	}

	var data []byte
	if file != "" {
		fmt.Printf("%s: read from %s, its payload is NOT checked against the upstream checksum\n", asset.Name, file)
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		data = b
	} else {
		b, err := fetch(asset)
		if err != nil {
			return err
		}
		data = b
	}

	sum := sha256.Sum256(data)
	base := goos + "-" + goarch
	if err := os.MkdirAll(filepath.Join("internal", "tailwind", "blobs"), 0o755); err != nil {
		return err
	}
	gzPath := filepath.Join("internal", "tailwind", "blobs", base+".gz")
	n, err := writeGzip(gzPath, data)
	if err != nil {
		return err
	}
	sumPath := filepath.Join("internal", "tailwind", "blobs", base+".sha256")
	if err := writeFile(sumPath, []byte(hex.EncodeToString(sum[:])+" "+fmt.Sprint(len(data))+"\n")); err != nil {
		return err
	}
	fmt.Printf("%s: %d bytes uncompressed, %d bytes compressed\n", asset.Name, len(data), n)
	return nil
}

// fetch returns the asset's bytes, from the build cache when the cached copy
// still hashes to the published checksum and from upstream otherwise.
func fetch(asset tailwind.Asset) ([]byte, error) {
	cachePath := ""
	if cacheDir, err := os.UserCacheDir(); err == nil {
		cachePath = filepath.Join(cacheDir, "htmlclay-build", "tailwind", tailwind.Version, asset.Name)
		if b, err := os.ReadFile(cachePath); err == nil {
			if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) == asset.SHA256 {
				return b, nil
			}
		}
	}

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(tailwind.AssetURL(asset.Name))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", tailwind.AssetURL(asset.Name), resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != asset.SHA256 {
		return nil, errors.New("downloaded binary does not match the published checksum")
	}
	if cachePath != "" {
		if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
			return nil, err
		}
		if err := writeFile(cachePath, data); err != nil {
			return nil, err
		}
	}
	return data, nil
}

// writeGzip writes data gzipped at the best level, with no name and a zero
// modified time, so the same input always produces the same bytes.
func writeGzip(path string, data []byte) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".blob-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	zw, err := gzip.NewWriterLevel(tmp, gzip.BestCompression)
	if err != nil {
		tmp.Close()
		return 0, err
	}
	zw.Name = ""
	zw.ModTime = time.Time{}
	if _, err := zw.Write(data); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := zw.Close(); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	info, err := os.Stat(tmp.Name())
	if err != nil {
		return 0, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func writeFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
