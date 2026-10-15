//go:build !darwin

package update

import "errors"

var errSparkleNotMac = errors.New("Sparkle installs on macOS only")

// SparkleParams describes one Sparkle install for BeginSparkle.
type SparkleParams struct {
	From        string
	To          string
	Bundle      string
	Executable  string
	OldPID      int
	HandoffPath string
	Quit        bool
}

func BeginSparkle(string, SparkleParams) (*State, error) { return nil, errSparkleNotMac }

func finishSparkleUpdate(dir string, st *State, _ SupervisorOptions, logf func(string, ...any)) error {
	logf("update %s was installed by Sparkle, which this platform does not run", st.Txid)
	return errSparkleNotMac
}
