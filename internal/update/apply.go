package update

import (
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// This file is the Windows and Linux self-update: stage the new binary beside the
// installed one, swap it in, and hand the restart to a supervisor that runs from
// the previous binary and rolls the swap back if the new version never reports
// that it is serving. macOS updates through Sparkle and never calls Apply.
//
// The state and health files below are a contract between two releases: version
// N's supervisor drives version N+1's startup. Both schemas are frozen from the
// first updater-capable release. Fields may be added; none may be renamed,
// removed or given a new meaning.

const (
	// StateSchema is the schema of updates/state.json.
	StateSchema = 1
	// HealthSchema is the schema of updates/health.json.
	HealthSchema = 1

	stateFile       = "state.json"
	healthFile      = "health.json"
	supervisorLog   = "supervisor.log"
	finishUpdateArg = "--finish-update"
)

// Phases of updates/state.json.
const (
	// PhaseApplying is written by the old process before it swaps the binary. It
	// is the only phase the supervisor acts on and the only one MarkHealth reports for.
	PhaseApplying = "applying"
	// PhaseDone means the new version reported serving. The previous binary stays
	// on disk until the next update replaces it.
	PhaseDone = "done"
	// PhaseRolledBack means the previous binary is back at the install path and
	// was relaunched; Failed says which version failed and why.
	PhaseRolledBack = "rolledBack"
	// PhaseRollbackFailed means the previous binary could not be moved back. The
	// supervisor still launched it from its backup path so the person has an app.
	PhaseRollbackFailed = "rollbackFailed"
)

// Phases of updates/health.json, written by the new process.
const (
	// HealthStarted is written once the new process holds the single-instance lock.
	HealthStarted = "started"
	// HealthServing is written once its sites are up and the update handoff is restored.
	HealthServing = "serving"
)

// stateMaxAge bounds how old an applying state the supervisor will act on. The
// old process spawns the supervisor seconds after writing it; an older one is a
// leftover from an update that died, and finishing it later would relaunch
// binaries nobody asked to start.
const stateMaxAge = 2 * time.Minute

// State is updates/state.json. Schema 1, frozen; extend additively only.
type State struct {
	Schema int `json:"schema"`
	// Phase is one of the Phase constants.
	Phase string `json:"phase"`
	// From and To are the running and the incoming version, strict X.Y.Z.
	From string `json:"from"`
	To   string `json:"to"`
	// OldPID is the process that applied the update and is about to exit.
	OldPID int `json:"oldPid"`
	// InstallPath is the binary the person launches, which now holds To.
	InstallPath string `json:"installPath"`
	// PreviousPath holds the From binary. The supervisor runs from it.
	PreviousPath string `json:"previousPath"`
	// Txid ties the health file to this update, so a health file left by an
	// earlier update can never vouch for this one.
	Txid string    `json:"txid"`
	Time time.Time `json:"time"`
	// Failed is set when the supervisor rolled To back. The updater skips that
	// version until a newer one appears.
	Failed *Failure `json:"failed,omitempty"`
}

type Failure struct {
	Version string    `json:"version"`
	Reason  string    `json:"reason"`
	Time    time.Time `json:"time"`
}

// Health is updates/health.json. Schema 1, frozen; extend additively only.
type Health struct {
	Schema  int       `json:"schema"`
	Txid    string    `json:"txid"`
	Version string    `json:"version"`
	PID     int       `json:"pid"`
	Phase   string    `json:"phase"`
	Time    time.Time `json:"time"`
}

// ErrNotWritable means the install folder cannot be written, so the update can
// only be announced, never applied: Program Files on Windows, a --system install
// in /usr/local/bin on Linux. The updater never asks for elevation.
var ErrNotWritable = errors.New("the install folder is not writable")

// UpdatesDir is where the updater keeps its state, inside the config dir.
func UpdatesDir(configDir string) string {
	return filepath.Join(configDir, "updates")
}

// InstallPath is the binary to replace: the running executable with symlinks
// resolved, so a symlinked launcher keeps pointing at the file that changes.
func InstallPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// CheckWritable reports whether the swap can happen in installPath's folder.
// Creating a file there is the real test; a mode check would miss ACLs and
// read-only mounts.
func CheckWritable(installPath string) error {
	f, err := os.CreateTemp(filepath.Dir(installPath), ".htmlclay-write-check-*")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotWritable, err)
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return nil
}

// Stage writes the new binary beside installPath, on the same filesystem, so the
// swap is a rename and never a copy. A .gz artifact (Linux) is decompressed; any
// other artifact (the Windows .exe) is copied as it is. The artifact has already
// passed its sha256 check, so this only has to be faithful.
func Stage(artifactPath, installPath string) (string, error) {
	src, err := os.Open(artifactPath)
	if err != nil {
		return "", err
	}
	defer src.Close()
	var r io.Reader = src
	if strings.HasSuffix(artifactPath, ".gz") {
		gz, err := gzip.NewReader(src)
		if err != nil {
			return "", fmt.Errorf("stage %s: %w", filepath.Base(artifactPath), err)
		}
		defer gz.Close()
		r = gz
	}

	staged := stagedPathFor(installPath)
	if err := os.Remove(staged); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	dst, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(dst, r)
	if err == nil {
		err = dst.Sync()
	}
	if closeErr := dst.Close(); err == nil {
		err = closeErr
	}
	// The umask may have stripped bits from the create mode.
	if err == nil {
		err = os.Chmod(staged, 0o755)
	}
	if err != nil {
		os.Remove(staged)
		return "", fmt.Errorf("stage %s: %w", filepath.Base(artifactPath), err)
	}
	return staged, nil
}

// ApplyParams describes one update for Prepare and Apply.
type ApplyParams struct {
	From        string
	To          string
	InstallPath string
	// StagedPath is what Stage returned.
	StagedPath string
	// OldPID is the process that will exit after Apply returns, normally os.Getpid().
	OldPID int
}

// Prepare records the update as applying and swaps the staged binary in. After
// it returns, InstallPath holds To and the returned state's PreviousPath holds
// From. On error nothing has moved and no state is left behind.
func Prepare(dir string, p ApplyParams) (*State, error) {
	if p.From == "" || p.To == "" || p.InstallPath == "" || p.StagedPath == "" || p.OldPID <= 0 {
		return nil, errors.New("update: incomplete apply parameters")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	txid := make([]byte, 16)
	if _, err := rand.Read(txid); err != nil {
		return nil, err
	}
	st := &State{
		Schema:       StateSchema,
		Phase:        PhaseApplying,
		From:         p.From,
		To:           p.To,
		OldPID:       p.OldPID,
		InstallPath:  p.InstallPath,
		PreviousPath: previousPathFor(p.InstallPath),
		Txid:         hex.EncodeToString(txid),
		Time:         time.Now().UTC(),
	}
	// State first: a new process that starts after the swap must find it.
	if err := writeJSONAtomic(filepath.Join(dir, stateFile), st); err != nil {
		return nil, err
	}
	if err := swapIn(p.StagedPath, p.InstallPath, st.PreviousPath); err != nil {
		os.Remove(filepath.Join(dir, stateFile))
		return nil, fmt.Errorf("swap: %w", err)
	}
	return st, nil
}

// Apply is Prepare plus starting the supervisor from the previous binary. When it
// returns nil the caller must shut down promptly: the supervisor is waiting for
// OldPID to exit. When the supervisor cannot start, the swap is undone and the
// caller keeps running the version it already is.
func Apply(dir string, p ApplyParams) (*State, error) {
	st, err := Prepare(dir, p)
	if err != nil {
		return nil, err
	}
	cmd, err := startDetached(st.PreviousPath, finishUpdateArg)
	if err != nil {
		if rbErr := os.Rename(st.PreviousPath, st.InstallPath); rbErr != nil {
			return nil, fmt.Errorf("start supervisor: %v; and could not undo the swap: %w", err, rbErr)
		}
		os.Remove(filepath.Join(dir, stateFile))
		return nil, fmt.Errorf("start supervisor: %w", err)
	}
	cmd.Process.Release()
	return st, nil
}

// ReadState returns updates/state.json, or nil when there is none.
func ReadState(dir string) (*State, error) {
	var st State
	ok, err := readJSON(filepath.Join(dir, stateFile), &st)
	if err != nil || !ok {
		return nil, err
	}
	if st.Schema != StateSchema {
		return nil, fmt.Errorf("update state schema %d is not supported", st.Schema)
	}
	return &st, nil
}

func readHealth(dir string) (*Health, error) {
	var h Health
	ok, err := readJSON(filepath.Join(dir, healthFile), &h)
	if err != nil || !ok {
		return nil, err
	}
	return &h, nil
}

// MarkHealth is the new process's half of the contract. It writes phase to
// updates/health.json only while state.json says an update to runningVersion is
// applying, so an ordinary launch, and the old version relaunched after a
// rollback, write nothing. It reads the state on every call rather than caching
// it, because the supervisor may finish or roll back between the two marks.
func MarkHealth(dir, runningVersion, phase string) error {
	st, err := ReadState(dir)
	if err != nil || st == nil {
		return err
	}
	if st.Phase != PhaseApplying || st.To != runningVersion {
		return nil
	}
	return writeJSONAtomic(filepath.Join(dir, healthFile), &Health{
		Schema:  HealthSchema,
		Txid:    st.Txid,
		Version: runningVersion,
		PID:     os.Getpid(),
		Phase:   phase,
		Time:    time.Now().UTC(),
	})
}

// SupervisorOptions are the supervisor's time limits. Zero means the default;
// tests shorten them.
type SupervisorOptions struct {
	// OldExitTimeout is how long to wait for the old process to exit before
	// killing it. Its shutdown drains HTTP for at most 3 s.
	OldExitTimeout time.Duration
	// ServingTimeout is how long the new version has, from launch, to report serving.
	ServingTimeout time.Duration
	// ExitGrace is how long an exited child gets for another process of the new
	// version to report started. The child exits on purpose when a launch of the
	// person's own (a double-click) took the single-instance lock first.
	ExitGrace time.Duration
	// Poll is the interval between health reads.
	Poll time.Duration
}

func (o SupervisorOptions) withDefaults() SupervisorOptions {
	if o.OldExitTimeout <= 0 {
		o.OldExitTimeout = 10 * time.Second
	}
	if o.ServingTimeout <= 0 {
		o.ServingTimeout = 20 * time.Second
	}
	if o.ExitGrace <= 0 {
		o.ExitGrace = 2 * time.Second
	}
	if o.Poll <= 0 {
		o.Poll = 100 * time.Millisecond
	}
	return o
}

// FinishUpdate is `htmlclay --finish-update`. It runs from the previous binary,
// detached from the old process, and takes nothing but dir's state.json as
// input: waits for the old process to exit, launches the new binary, and waits
// for it to report serving. If it does not, the supervisor kills it, moves the
// previous binary back, records the failure and relaunches the previous version.
// It returns nil only when the update finished.
func FinishUpdate(dir string, opts SupervisorOptions) error {
	opts = opts.withDefaults()
	logf := openSupervisorLog(dir)

	st, err := ReadState(dir)
	if err != nil {
		logf("cannot read the update state: %v", err)
		return err
	}
	if st == nil || st.Phase != PhaseApplying {
		logf("no update is applying; nothing to finish")
		return errors.New("no update is applying")
	}
	if age := time.Since(st.Time); age < 0 || age > stateMaxAge {
		logf("update %s to %s is stale (written %s); leaving it", st.Txid, st.To, st.Time.Format(time.RFC3339))
		return fmt.Errorf("update state %s is stale", st.Txid)
	}
	logf("finishing update %s: %s -> %s at %s", st.Txid, st.From, st.To, st.InstallPath)

	if !waitGone(st.OldPID, opts.OldExitTimeout) {
		// The old process already swapped the binary and closed its sessions, so
		// it has nothing left to protect, and while it lives it holds the lock
		// and the port the new version needs.
		logf("old process %d did not exit within %s; killing it", st.OldPID, opts.OldExitTimeout)
		killPID(st.OldPID)
		waitGone(st.OldPID, 2*time.Second)
	}

	reason := watchNewVersion(dir, st, opts, logf)
	if reason == "" {
		st.Phase = PhaseDone
		if err := writeJSONAtomic(filepath.Join(dir, stateFile), st); err != nil {
			logf("update %s is serving but the state could not be saved: %v", st.Txid, err)
		}
		logf("update %s finished: %s is serving", st.Txid, st.To)
		return nil
	}

	logf("rolling back update %s: %s", st.Txid, reason)
	st.Failed = &Failure{Version: st.To, Reason: reason, Time: time.Now().UTC()}
	relaunch := st.InstallPath
	if err := os.Rename(st.PreviousPath, st.InstallPath); err != nil {
		logf("could not move %s back to %s: %v", st.PreviousPath, st.InstallPath, err)
		st.Phase = PhaseRollbackFailed
		relaunch = st.PreviousPath
	} else {
		st.Phase = PhaseRolledBack
	}
	if err := writeJSONAtomic(filepath.Join(dir, stateFile), st); err != nil {
		logf("could not save the rollback state: %v", err)
	}
	if cmd, err := startDetached(relaunch); err != nil {
		logf("could not relaunch %s: %v", relaunch, err)
	} else {
		cmd.Process.Release()
		logf("relaunched %s from %s", st.From, relaunch)
	}
	return fmt.Errorf("update to %s rolled back: %s", st.To, reason)
}

// watchNewVersion launches the new binary and returns "" once it reports serving,
// or the reason it failed after making sure no process of the new version is left
// running.
func watchNewVersion(dir string, st *State, opts SupervisorOptions, logf func(string, ...any)) string {
	cmd, err := startDetached(st.InstallPath)
	if err != nil {
		return fmt.Sprintf("could not launch %s: %v", st.To, err)
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()
	childPID := cmd.Process.Pid
	watched := childPID
	logf("launched %s as pid %d", st.To, childPID)

	deadline := time.Now().Add(opts.ServingTimeout)
	var exitedAt time.Time
	for {
		if h, _ := readHealth(dir); h != nil && h.Schema == HealthSchema && h.Txid == st.Txid && h.Version == st.To {
			if h.Phase == HealthServing {
				return ""
			}
			// The person's own launch of the new version won the lock and our
			// child forwarded to it; that process is the one to judge.
			if h.Phase == HealthStarted && h.PID != watched && h.PID > 0 && processAlive(h.PID) {
				logf("pid %d of %s reported started; watching it instead of %d", h.PID, st.To, watched)
				watched = h.PID
			}
		}

		alive := true
		if watched == childPID {
			select {
			case <-exited:
				alive = false
			default:
			}
		} else {
			alive = processAlive(watched)
		}
		if !alive {
			if exitedAt.IsZero() {
				exitedAt = time.Now()
			}
			if watched != childPID || time.Since(exitedAt) >= opts.ExitGrace {
				return fmt.Sprintf("%s exited before it reported serving", st.To)
			}
		}
		if time.Now().After(deadline) {
			reason := fmt.Sprintf("%s did not report serving within %s", st.To, opts.ServingTimeout)
			stopNewVersion(cmd, exited, watched, childPID)
			return reason
		}
		time.Sleep(opts.Poll)
	}
}

// stopNewVersion kills whatever process of the new version is still running, so
// the rollback never moves a binary out from under a live process and the old
// version can take the lock and the port.
func stopNewVersion(cmd *exec.Cmd, exited <-chan struct{}, watched, childPID int) {
	if watched != childPID {
		killPID(watched)
		waitGone(watched, 5*time.Second)
	}
	cmd.Process.Kill()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
	}
}

// waitGone polls until pid has exited or timeout passes, and reports whether it exited.
func waitGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

func killPID(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		p.Kill()
	}
}

// openSupervisorLog appends to updates/supervisor.log. The supervisor is
// detached with no console, so this file is the only record of what it decided.
func openSupervisorLog(dir string) func(string, ...any) {
	f, err := os.OpenFile(filepath.Join(dir, supervisorLog), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return func(string, ...any) {}
	}
	l := log.New(f, fmt.Sprintf("[supervisor %d] ", os.Getpid()), log.LstdFlags)
	return func(format string, args ...any) { l.Printf(format, args...) }
}

func readJSON(path string, v any) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return true, nil
}

// writeJSONAtomic writes through a temp file and a rename, so a reader in the
// other process sees the old file or the new one, never half of one. The rename
// is retried because Windows refuses to replace a file another process has open
// at that instant, and both processes poll these files.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
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
	for attempt := 0; ; attempt++ {
		err = os.Rename(tmpPath, path)
		if err == nil || attempt == 20 {
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
}
