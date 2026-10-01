package aiedit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxContextRefs  = 8
	maxContextFile  = 256 << 10
	maxContextTotal = 1 << 20
)

// resolveContext turns @ref tokens into prompt sections. Every ref has to land
// on a regular file inside the document's own folder, after symlinks.
func resolveContext(refs []string, baseDir string) ([]string, error) {
	if len(refs) > maxContextRefs {
		return nil, contextFailure("too many context files, at most %d", maxContextRefs)
	}
	base, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		base = filepath.Clean(baseDir)
	}
	sections := make([]string, 0, len(refs))
	total := 0
	for _, ref := range refs {
		if ref == "page" {
			continue
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(baseDir, ref))
		if err != nil {
			return nil, contextFailure("cannot read @%s", ref)
		}
		if !insideDir(base, resolved) {
			return nil, contextFailure("@%s escapes the document's folder", ref)
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			return nil, contextFailure("cannot read @%s", ref)
		}
		if info.Size() > maxContextFile {
			return nil, contextFailure("@%s is larger than 256 KB", ref)
		}
		total += int(info.Size())
		if total > maxContextTotal {
			return nil, contextFailure("@%s pushes the context files over 1 MB in total", ref)
		}
		content, err := os.ReadFile(resolved)
		if err != nil {
			return nil, contextFailure("cannot read @%s", ref)
		}
		sections = append(sections, fmt.Sprintf("Context file @%s:\n\n%s", ref, string(content)))
	}
	return sections, nil
}

// insideDir reports whether path is strictly inside dir.
func insideDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == "." {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func contextFailure(format string, args ...any) *Error {
	return &Error{Code: "invalid_context", Message: fmt.Sprintf(format, args...)}
}
