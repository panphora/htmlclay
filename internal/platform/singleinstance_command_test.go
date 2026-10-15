//go:build !windows

package platform

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortConfigDir keeps the socket path under the 104-byte limit macOS puts on
// Unix socket paths, which t.TempDir() can exceed.
func shortConfigDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "htmlclay-si-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func primaryForTest(t *testing.T) (SingleInstance, string) {
	t.Helper()
	dir := shortConfigDir(t)
	si := NewSingleInstance(dir)
	ok, err := si.TryLock()
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	t.Cleanup(func() { si.Unlock() })
	return si, dir
}

func TestSingleInstanceCommandIsAnsweredAndNeverOpenedAsAFile(t *testing.T) {
	si, dir := primaryForTest(t)
	opened := make(chan string, 1)
	si.OnFileReceived(func(p string) { opened <- p })
	got := make(chan string, 1)
	si.OnCommand(func(cmd string) string { got <- cmd; return "accepted" })

	answer, err := NewSingleInstance(dir).SendCommand("update-now 9.9.9")
	if err != nil || answer != "accepted" {
		t.Fatalf("SendCommand = %q, %v; want accepted", answer, err)
	}
	if cmd := <-got; cmd != "update-now 9.9.9" {
		t.Fatalf("handler got %q", cmd)
	}
	select {
	case p := <-opened:
		t.Fatalf("a command frame was opened as the file %q", p)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSingleInstanceFilePathStillOpens(t *testing.T) {
	si, dir := primaryForTest(t)
	opened := make(chan string, 1)
	si.OnFileReceived(func(p string) { opened <- p })
	si.OnCommand(func(string) string { t.Error("a file path reached the command handler"); return "" })

	if err := NewSingleInstance(dir).SendFilePath("/Users/x/notes.htmlclay"); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-opened:
		if p != "/Users/x/notes.htmlclay" {
			t.Fatalf("opened %q", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the file path was never delivered")
	}
}

func TestSingleInstanceCommandBeforeAHandlerAnswersStarting(t *testing.T) {
	_, dir := primaryForTest(t)
	answer, err := NewSingleInstance(dir).SendCommand("update-now 9.9.9")
	if err != nil || answer != CommandStarting {
		t.Fatalf("SendCommand = %q, %v; want %q", answer, err, CommandStarting)
	}
}

func TestSingleInstanceCommandFromAnotherUserIsRefused(t *testing.T) {
	defer func(f func(net.Conn) bool) { peerIsSelf = f }(peerIsSelf)
	peerIsSelf = func(net.Conn) bool { return false }
	si, dir := primaryForTest(t)
	si.OnCommand(func(string) string { t.Error("another user's command reached the handler"); return "accepted" })

	answer, err := NewSingleInstance(dir).SendCommand("update-now 9.9.9")
	if err != nil || answer != "refused the command did not come from this user" {
		t.Fatalf("SendCommand = %q, %v", answer, err)
	}
}

func TestSingleInstancePeerIsThisUser(t *testing.T) {
	_, dir := primaryForTest(t)
	ln, err := net.Listen("unix", filepath.Join(dir, "peer"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := net.Dial("unix", filepath.Join(dir, "peer"))
		if err == nil {
			time.Sleep(200 * time.Millisecond)
			c.Close()
		}
	}()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !peerUIDIsSelf(c) {
		t.Fatal("a connection from this process was not recognised as this user")
	}
}

func TestSingleInstanceMalformedCommandIsRefused(t *testing.T) {
	si, dir := primaryForTest(t)
	si.OnCommand(func(string) string { t.Error("a malformed frame reached the handler"); return "accepted" })
	for name, frame := range map[string]string{
		"too long":     "\x00update-now " + strings.Repeat("9", 300) + "\n",
		"control byte": "\x00update-now\t9.9.9\n",
		"empty":        "\x00\n",
	} {
		c, err := net.Dial("unix", filepath.Join(dir, "sock"))
		if err != nil {
			t.Fatal(err)
		}
		if !verifyBanner(c) {
			t.Fatal("no banner")
		}
		io.WriteString(c, frame)
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf, _ := io.ReadAll(io.LimitReader(c, 1024))
		c.Close()
		if string(buf) != "refused malformed command\n" {
			t.Errorf("%s: answer %q", name, buf)
		}
	}
}

// A build from before command frames reads until the peer closes and then
// opens what it read. SendCommand never closes first, so that build hits its
// own read deadline, drops the frame and closes, and SendCommand reports
// ErrNoAnswer.
func TestSingleInstanceOldPrimaryGivesNoAnswer(t *testing.T) {
	defer func(d time.Duration) { commandAnswerTimeout = d }(commandAnswerTimeout)
	commandAnswerTimeout = 2 * time.Second
	dir := shortConfigDir(t)
	ln, err := net.Listen("unix", filepath.Join(dir, "sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	opened := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		writeBanner(c)
		c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		data, err := io.ReadAll(c)
		if err == nil && len(data) > 0 {
			opened <- data
		}
	}()

	_, err = NewSingleInstance(dir).SendCommand("update-now 9.9.9")
	if !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("SendCommand error %v, want ErrNoAnswer", err)
	}
	select {
	case data := <-opened:
		t.Fatalf("the old primary would have opened %q", data)
	default:
	}
}

func TestSingleInstanceCommandWithNothingRunningFails(t *testing.T) {
	if _, err := NewSingleInstance(shortConfigDir(t)).SendCommand("update-now 9.9.9"); err == nil || errors.Is(err, ErrNoAnswer) {
		t.Fatalf("SendCommand with no primary = %v, want a dial error", err)
	}
}
