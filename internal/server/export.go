package server

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ExportDocumentZip writes the document and its uploads folder into a new zip in
// outDir and returns the zip's path. Both go under one top folder named after the
// document, so unzipping anywhere gives back a folder whose relative links still
// resolve. Hidden entries (an interrupted upload's temp file) and symlinks are
// left out. The zip is written beside its final name and renamed into place, and
// an existing zip is never replaced: "board 2.zip" and so on.
func ExportDocumentZip(document, outDir string) (string, error) {
	info, err := os.Stat(document)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a file", document)
	}
	assets, folder := assetsDirFor(document)
	top := strings.TrimPrefix(folder, "assets-")

	out, err := freeZipPath(outDir, top)
	if err != nil {
		return "", err
	}
	part := out + ".part"
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(part)
		}
	}()

	zw := zip.NewWriter(f)
	if err := addZipFile(zw, document, top+"/"+filepath.Base(document)); err != nil {
		return "", err
	}
	walkErr := filepath.WalkDir(assets, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == assets && errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipDir
			}
			return err
		}
		if strings.HasPrefix(d.Name(), ".") && p != assets {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(assets, p)
		if err != nil {
			return err
		}
		return addZipFile(zw, p, top+"/"+folder+"/"+filepath.ToSlash(rel))
	})
	if walkErr != nil {
		return "", walkErr
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(part, out); err != nil {
		os.Remove(part)
		return "", err
	}
	ok = true
	return out, nil
}

func addZipFile(zw *zip.Writer, src, name string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	header.Name = name
	header.Method = zip.Deflate
	w, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, in)
	return err
}

func freeZipPath(dir, stem string) (string, error) {
	for n := 1; n < 1000; n++ {
		name := stem + ".zip"
		if n > 1 {
			name = fmt.Sprintf("%s %d.zip", stem, n)
		}
		p := filepath.Join(dir, name)
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			if _, err := os.Lstat(p + ".part"); errors.Is(err, fs.ErrNotExist) {
				return p, nil
			}
		}
	}
	return "", errors.New("no free name for the zip")
}
