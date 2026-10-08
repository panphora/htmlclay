package server

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// uploadsURLPrefix is how a document links a file it uploaded. It is a host
// path: the site server answers it out of the per-computer library, and only a
// host can. The exported copy of the document says uploads/ instead, which
// resolves inside the zip wherever it is unpacked.
const uploadsURLPrefix = "/_/uploads/"

// ExportDocumentZip writes the document and its uploads folder into a new zip in
// outDir and returns the zip's path. Both go under one top folder named after the
// document, so unzipping anywhere gives back a folder whose relative links still
// resolve. Hidden entries (an interrupted upload's temp file) and symlinks are
// left out. The zip is written beside its final name and renamed into place, and
// an existing zip is never replaced: "board 2.zip" and so on.
//
// Every file the document links through /_/uploads/ is packaged from uploadsDir,
// the per-computer library, under <top>/uploads/, and the document written into
// the zip links it as uploads/... instead of the host path, which resolves
// nowhere but on the computer that exported it. An empty uploadsDir packages no
// library file and still rewrites the links. The document on disk is only read.
func ExportDocumentZip(document, outDir, uploadsDir string) (string, error) {
	info, err := os.Stat(document)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a file", document)
	}
	assets, folder := assetsDirFor(document)
	top := strings.TrimPrefix(folder, "assets-")

	doc, err := os.ReadFile(document)
	if err != nil {
		return "", err
	}
	refs := hostUploadRefs(doc)

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
	if len(refs) == 0 {
		if err := addZipFile(zw, document, top+"/"+filepath.Base(document)); err != nil {
			return "", err
		}
	} else {
		if err := addZipUploads(zw, uploadsDir, top, refs); err != nil {
			return "", err
		}
		if err := addZipBytes(zw, info, top+"/"+filepath.Base(document), rewriteHostUploadRefs(doc, refs)); err != nil {
			return "", err
		}
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

// addZipUploads packages every distinct referenced file out of the uploads
// library under <top>/uploads/<its path in the library>. os.Root is the
// containment: nothing outside the library can be read, whatever the reference
// spells. Only a regular file is packaged, so a symlink out of the library is
// left behind and a file that is missing or refused is simply not in the zip. A
// library that is not there at all packages nothing rather than failing the
// export: the person who never uploaded anything still has a document to send.
func addZipUploads(zw *zip.Writer, uploadsDir, top string, refs []hostUploadRef) error {
	if uploadsDir == "" {
		return nil
	}
	root, err := os.OpenRoot(uploadsDir)
	if err != nil {
		return nil
	}
	defer root.Close()
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		rel := strings.Join(ref.segments, "/")
		if seen[rel] {
			continue
		}
		seen[rel] = true
		info, err := root.Lstat(rel)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		in, err := root.Open(rel)
		if err != nil {
			continue
		}
		err = addZipBytesFrom(zw, info, top+"/uploads/"+rel, in)
		in.Close()
		if err != nil {
			return err
		}
	}
	return nil
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
	return addZipBytesFrom(zw, info, name, in)
}

func addZipBytes(zw *zip.Writer, info fs.FileInfo, name string, data []byte) error {
	return addZipBytesFrom(zw, info, name, bytes.NewReader(data))
}

func addZipBytesFrom(zw *zip.Writer, info fs.FileInfo, name string, r io.Reader) error {
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
	_, err = io.Copy(w, r)
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

// hostUploadRef is one host-path reference to a library file in a document: the
// bytes it occupies and the library path it names.
type hostUploadRef struct {
	start    int
	end      int
	segments []string
}

// hostUploadRefs finds every reference to /_/uploads/ in a document, by the same
// rule hyperclay.com and Hyperclay Local match one with, so the three agree on
// what a link is. An occurrence counts only where it stands free: the byte
// before it is absent or not part of a URL token, which keeps the tail of
// https://x.hyperclay.com/_/uploads/... from being read as one. The reference
// then runs right while the bytes could belong to a URL path.
//
// A reference is accepted only if it names a file at least one folder deep in
// the library: every segment, unescaped, has to be a real name, so "..", a
// hidden name and an escaped slash are all refused. Nothing here touches the
// disk; what is packaged is decided by the caller.
func hostUploadRefs(doc []byte) []hostUploadRef {
	var refs []hostUploadRef
	for i := 0; i+len(uploadsURLPrefix) <= len(doc); i++ {
		if !bytes.HasPrefix(doc[i:], []byte(uploadsURLPrefix)) {
			continue
		}
		if i > 0 && uploadRefByte(doc[i-1]) {
			continue
		}
		end := i + len(uploadsURLPrefix)
		for end < len(doc) && uploadRefByte(doc[end]) {
			end++
		}
		segments, ok := uploadRefSegments(string(doc[i+len(uploadsURLPrefix) : end]))
		if !ok {
			continue
		}
		refs = append(refs, hostUploadRef{start: i, end: end, segments: segments})
		i = end - 1
	}
	return refs
}

// uploadRefByte reports whether a byte may be part of a URL path, the alphabet
// the reference extends over on both sides.
func uploadRefByte(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	case b == '.', b == '_', b == '~', b == '%', b == '/', b == '-':
		return true
	}
	return false
}

// uploadRefSegments turns the part of a reference after /_/uploads/ into the
// library path it names. The folder is what makes it a library file: one flat
// name is not. Each segment is unescaped before it is judged, so %2e%2e is ".."
// and is refused like any other escape out of the library.
func uploadRefSegments(rel string) ([]string, bool) {
	parts := strings.Split(rel, "/")
	if len(parts) < 2 {
		return nil, false
	}
	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		segment, err := url.PathUnescape(part)
		if err != nil {
			return nil, false
		}
		if segment == "" || segment == "." || segment == ".." ||
			strings.HasPrefix(segment, ".") || strings.ContainsAny(segment, `/\`) {
			return nil, false
		}
		segments = append(segments, segment)
	}
	return segments, true
}

// rewriteHostUploadRefs is the document as it goes into the zip: every accepted
// reference says uploads/... instead of /_/uploads/..., so the copy opens on any
// computer and its attachments resolve beside it. The bytes of the reference
// itself, escapes and all, are kept as the document wrote them.
func rewriteHostUploadRefs(doc []byte, refs []hostUploadRef) []byte {
	out := make([]byte, 0, len(doc))
	last := 0
	for _, ref := range refs {
		out = append(out, doc[last:ref.start]...)
		out = append(out, "uploads/"...)
		out = append(out, doc[ref.start+len(uploadsURLPrefix):ref.end]...)
		last = ref.end
	}
	return append(out, doc[last:]...)
}
