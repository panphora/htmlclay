package logging

import (
	"fmt"
	"os"
	"sync"
	"time"
)

const maxLogSize = 10 * 1024 * 1024

type Logger struct {
	mu        sync.Mutex
	file      *os.File
	path      string
	written   int64
	ownsFile  bool
	teeStderr bool
}

func New(path string) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}

	return &Logger{
		file:     f,
		path:     path,
		written:  info.Size(),
		ownsFile: true,
	}, nil
}

func NewDualWrite(path string) (*Logger, error) {
	l, err := New(path)
	if err != nil {
		return nil, err
	}
	l.teeStderr = true
	return l, nil
}

func NewStdout() *Logger {
	return &Logger{
		file: os.Stdout,
	}
}

func (l *Logger) Printf(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("%s %s\n", time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), msg)

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		return
	}

	if l.teeStderr && l.file != os.Stderr {
		fmt.Fprint(os.Stderr, line)
	}

	if _, err := l.file.WriteString(line); err != nil {
		return
	}
	l.written += int64(len(line))

	if l.ownsFile && l.written >= maxLogSize {
		l.rotate()
	}
}

func (l *Logger) rotate() {
	l.file.Close()
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		fmt.Fprintf(os.Stderr, "[htmlclay] log rotation rename failed: %v\n", err)
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[htmlclay] log rotation failed, falling back to stderr: %v\n", err)
		l.file = os.Stderr
		l.ownsFile = false
		l.written = 0
		return
	}
	l.file = f
	l.written = 0
}

func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ownsFile && l.file != nil {
		l.file.Close()
		l.file = nil
	}
}
