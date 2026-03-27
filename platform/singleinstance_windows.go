//go:build windows

package platform

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type tcpSingleInstance struct {
	configDir string
	addrFile  string
	listener  net.Listener
	mu        sync.Mutex
	callback  func(string)
	pending   []string
}

func NewSingleInstance(configDir string) SingleInstance {
	return &tcpSingleInstance{
		configDir: configDir,
		addrFile:  filepath.Join(configDir, "addr"),
	}
}

func (s *tcpSingleInstance) TryLock() (bool, error) {
	data, err := os.ReadFile(s.addrFile)
	if err == nil {
		addr := strings.TrimSpace(string(data))
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return false, nil
		}
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return false, fmt.Errorf("cannot create listener: %w", err)
	}
	s.listener = listener

	if err := os.WriteFile(s.addrFile, []byte(listener.Addr().String()), 0644); err != nil {
		listener.Close()
		return false, fmt.Errorf("cannot write addr file: %w", err)
	}

	go s.acceptLoop()
	return true, nil
}

func (s *tcpSingleInstance) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConnection(conn)
	}
}

func (s *tcpSingleInstance) handleConnection(c net.Conn) {
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	data, err := io.ReadAll(io.LimitReader(c, 64<<10))
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

func (s *tcpSingleInstance) SendFilePath(path string) error {
	data, err := os.ReadFile(s.addrFile)
	if err != nil {
		return fmt.Errorf("cannot read addr file: %w", err)
	}
	addr := strings.TrimSpace(string(data))
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(path))
	return err
}

func (s *tcpSingleInstance) OnFileReceived(callback func(string)) {
	s.mu.Lock()
	s.callback = callback
	queued := s.pending
	s.pending = nil
	s.mu.Unlock()

	for _, path := range queued {
		callback(path)
	}
}

func (s *tcpSingleInstance) Unlock() error {
	if s.listener != nil {
		s.listener.Close()
	}
	os.Remove(s.addrFile)
	return nil
}
