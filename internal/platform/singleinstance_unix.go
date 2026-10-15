//go:build !windows

package platform

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type socketSingleInstance struct {
	sockPath string
	lockPath string
	listener net.Listener
	mu       sync.Mutex
	callback func(string)
	pending  []string
	command  func(string) string
}

// peerIsSelf reports whether the process on the other end of c runs as this
// user. Tests replace it.
var peerIsSelf = peerUIDIsSelf

func NewSingleInstance(configDir string) SingleInstance {
	return &socketSingleInstance{
		sockPath: filepath.Join(configDir, "sock"),
		lockPath: filepath.Join(configDir, "lock"),
	}
}

func (s *socketSingleInstance) TryLock() (bool, error) {
	conn, err := net.Dial("unix", s.sockPath)
	if err == nil {
		alive := verifyBanner(conn)
		conn.Close()
		if alive {
			return false, nil
		}
	}

	if s.isExistingInstanceAlive() {
		return false, nil
	}

	os.Remove(s.sockPath)

	listener, err := net.Listen("unix", s.sockPath)
	if err != nil {
		return false, fmt.Errorf("cannot create socket: %w", err)
	}
	s.listener = listener

	lockData := fmt.Sprintf("%d\n", os.Getpid())
	if err := os.WriteFile(s.lockPath, []byte(lockData), 0644); err != nil {
		listener.Close()
		return false, fmt.Errorf("cannot write lock file: %w", err)
	}

	go s.acceptLoop()

	return true, nil
}

func (s *socketSingleInstance) isExistingInstanceAlive() bool {
	data, err := os.ReadFile(s.lockPath)
	if err != nil {
		return false
	}

	pidStr := strings.TrimSpace(strings.Split(string(data), "\n")[0])
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return false
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	if process.Signal(syscall.Signal(0)) != nil {
		os.Remove(s.sockPath)
		os.Remove(s.lockPath)
		return false
	}

	conn, err := net.Dial("unix", s.sockPath)
	if err != nil {
		os.Remove(s.sockPath)
		os.Remove(s.lockPath)
		return false
	}
	alive := verifyBanner(conn)
	conn.Close()
	if !alive {
		os.Remove(s.sockPath)
		os.Remove(s.lockPath)
		return false
	}
	return true
}

func (s *socketSingleInstance) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConnection(conn)
	}
}

func (s *socketSingleInstance) handleConnection(c net.Conn) {
	defer c.Close()
	writeBanner(c)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(io.LimitReader(c, 64<<10))
	if isCommandFrame(r) {
		refusal := ""
		if !peerIsSelf(c) {
			refusal = "the command did not come from this user"
		}
		s.mu.Lock()
		handler := s.command
		s.mu.Unlock()
		answerCommand(c, r, refusal, handler)
		return
	}
	data, err := io.ReadAll(r)
	if err != nil || len(data) == 0 {
		return
	}
	path := string(data)

	s.mu.Lock()
	if s.callback != nil {
		cb := s.callback
		s.mu.Unlock()
		cb(path)
	} else {
		s.pending = append(s.pending, path)
		s.mu.Unlock()
	}
}

func (s *socketSingleInstance) SendFilePath(path string) error {
	conn, err := net.Dial("unix", s.sockPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	if !verifyBanner(conn) {
		return fmt.Errorf("peer on %s is not an htmlclay instance", s.sockPath)
	}
	_, err = conn.Write([]byte(path))
	return err
}

func (s *socketSingleInstance) SendCommand(cmd string) (string, error) {
	conn, err := net.DialTimeout("unix", s.sockPath, time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if !verifyBanner(conn) {
		return "", fmt.Errorf("peer on %s is not an htmlclay instance", s.sockPath)
	}
	return exchangeCommand(conn, cmd)
}

func (s *socketSingleInstance) OnCommand(handler func(string) string) {
	s.mu.Lock()
	s.command = handler
	s.mu.Unlock()
}

func (s *socketSingleInstance) OnFileReceived(callback func(string)) {
	s.mu.Lock()
	s.callback = callback
	queued := s.pending
	s.pending = nil
	s.mu.Unlock()

	for _, path := range queued {
		callback(path)
	}
}

func (s *socketSingleInstance) Unlock() error {
	if s.listener != nil {
		s.listener.Close()
	}
	os.Remove(s.sockPath)
	os.Remove(s.lockPath)
	return nil
}
