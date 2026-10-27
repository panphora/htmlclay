package versions

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"regexp"
)

// PageStateName is the sidecar inside a document's history folder. It never
// parses as a version entry, so List, Read and pruning never see it.
const PageStateName = "page-state.json"

// MaxPageStateTag bounds one stored page tag.
const MaxPageStateTag = 32 << 20

var (
	hexHash       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hexGeneration = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// PageStateRecord is the page editor's merge state for one exact file. FileHash
// is Hash of the bytes handed to the file writer; StateHash is Hash of the exact
// Tag bytes; Generation is the page lineage this state belongs to.
type PageStateRecord struct {
	Version    int    `json:"version"`
	FileHash   string `json:"fileHash"`
	Generation string `json:"generation"`
	StateHash  string `json:"stateHash"`
	Tag        string `json:"tag"`
}

// The sidecar holds at most the record for the bytes on disk when it was
// written, and one prepared candidate for a write that may not have landed.
type pageStateFile struct {
	Version   int              `json:"version"`
	Matching  *PageStateRecord `json:"matching,omitempty"`
	Candidate *PageStateRecord `json:"candidate,omitempty"`
}

var ErrInvalidPageState = errors.New("invalid page state record")

// NewPageGeneration returns a fresh page lineage token.
func NewPageGeneration() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// NewPageStateRecord builds a record for tag stored with the file whose bytes hash to fileHash.
func NewPageStateRecord(fileHash, generation, tag string) PageStateRecord {
	return PageStateRecord{Version: 1, FileHash: fileHash, Generation: generation, StateHash: Hash([]byte(tag)), Tag: tag}
}

func (r PageStateRecord) valid() bool {
	return r.Version == 1 && hexHash.MatchString(r.FileHash) && hexGeneration.MatchString(r.Generation) &&
		len(r.Tag) > 0 && len(r.Tag) <= MaxPageStateTag && r.StateHash == Hash([]byte(r.Tag))
}

// readPageState reads the sidecar through the opened history root. Anything that
// is not a version 1 sidecar of regular file provenance reads as empty, and a
// slot that does not validate is dropped rather than trusted.
func readPageState(root *os.Root) pageStateFile {
	f, err := root.Open(PageStateName)
	if err != nil {
		return pageStateFile{}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return pageStateFile{}
	}
	if info.Size() > 2*int64(MaxPageStateTag)+4096 {
		return pageStateFile{}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return pageStateFile{}
	}
	var ps pageStateFile
	if err := json.Unmarshal(data, &ps); err != nil || ps.Version != 1 {
		return pageStateFile{}
	}
	if ps.Matching != nil && !ps.Matching.valid() {
		ps.Matching = nil
	}
	if ps.Candidate != nil && !ps.Candidate.valid() {
		ps.Candidate = nil
	}
	return ps
}

// PreparePageState durably writes candidate beside the record for the bytes on
// disk (diskHash, "" when there is no file), before the caller writes the file.
// A candidate for the same bytes as disk replaces that record outright: there
// is no file write left to fail. An earlier candidate that never landed is
// dropped, never promoted.
func (s *Store) PreparePageState(key, absPath, diskHash string, candidate PageStateRecord) error {
	if !candidate.valid() {
		return ErrInvalidPageState
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	vroot, err := s.openVersionsRoot(true)
	if err != nil {
		return err
	}
	defer vroot.Close()
	ref, err := s.openHistory(vroot, key, absPath, true)
	if err != nil {
		return err
	}
	defer ref.Close()

	current := readPageState(ref.root)
	var kept *PageStateRecord
	if diskHash != "" {
		switch {
		case current.Matching != nil && current.Matching.FileHash == diskHash:
			kept = current.Matching
		case current.Candidate != nil && current.Candidate.FileHash == diskHash:
			kept = current.Candidate
		}
	}

	next := pageStateFile{Version: 1}
	if candidate.FileHash == diskHash {
		copied := candidate
		next.Matching = &copied
	} else {
		next.Matching = kept
		copied := candidate
		next.Candidate = &copied
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return atomicPublishBytes(ref.root, PageStateName, data)
}

// PageStateFor returns the stored record whose FileHash equals fileHash.
func (s *Store) PageStateFor(key, absPath, fileHash string) (PageStateRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	vroot, err := s.openVersionsRoot(false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return PageStateRecord{}, false, nil
		}
		return PageStateRecord{}, false, err
	}
	defer vroot.Close()
	ref, err := s.openHistory(vroot, key, absPath, false)
	if err != nil {
		if errors.Is(err, ErrNoHistory) || errors.Is(err, fs.ErrNotExist) {
			return PageStateRecord{}, false, nil
		}
		return PageStateRecord{}, false, err
	}
	defer ref.Close()

	ps := readPageState(ref.root)
	if ps.Matching != nil && ps.Matching.FileHash == fileHash {
		return *ps.Matching, true, nil
	}
	if ps.Candidate != nil && ps.Candidate.FileHash == fileHash {
		return *ps.Candidate, true, nil
	}
	return PageStateRecord{}, false, nil
}

// SettlePageState runs after the file write: it keeps only the record for the
// bytes now on disk, and removes the sidecar when no record matches them.
func (s *Store) SettlePageState(key, absPath, diskHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	vroot, err := s.openVersionsRoot(false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer vroot.Close()
	ref, err := s.openHistory(vroot, key, absPath, false)
	if err != nil {
		if errors.Is(err, ErrNoHistory) || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer ref.Close()

	current := readPageState(ref.root)
	var match *PageStateRecord
	switch {
	case current.Matching != nil && current.Matching.FileHash == diskHash:
		match = current.Matching
	case current.Candidate != nil && current.Candidate.FileHash == diskHash:
		match = current.Candidate
	}
	if match == nil {
		if err := ref.root.Remove(PageStateName); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if current.Matching == match && current.Candidate == nil {
		return nil
	}
	data, err := json.Marshal(pageStateFile{Version: 1, Matching: match})
	if err != nil {
		return err
	}
	return atomicPublishBytes(ref.root, PageStateName, data)
}
