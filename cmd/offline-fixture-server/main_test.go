package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/panphora/htmlclay/internal/clayruntime"
)

// kanbanFixture is the kanban template's ClayJS head tags: the three public
// runtime URLs a host must project onto its own copy of the release.
const kanbanFixture = `<!DOCTYPE html>
<html lang="en">
<head>
<script src="https://clayjs.com/v1/clay.js?plugins=sortable,people,upload,sync,visitor-drafts,unsent,menu&exclude=richclay"></script>
<script src="https://clayjs.com/v1/sap.js"></script>
<link rel="stylesheet" href="https://clayjs.com/v1/src/page/themes/light-1.css">
</head>
<body><p>board</p></body>
</html>
`

// plainFixture links no runtime, so serving it must change no byte but the
// host's own id, token and stamp.
const plainFixture = `<!DOCTYPE html>
<html lang="en"><head><title>Note</title></head><body><p>note</p></body></html>
`

type fixtureServer struct {
	baseURL   string
	runtime   string
	documents map[string]document
	stdout    *bufio.Reader
	stdin     io.WriteCloser
	cmd       *exec.Cmd
	stderr    *bytes.Buffer
}

func buildFixtureServer(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "offline-fixture-server")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func writeFixtures(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "notes", "2026"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "board.html"), []byte(kanbanFixture), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes", "2026", "doc.html"), []byte(plainFixture), 0644); err != nil {
		t.Fatal(err)
	}
	return root
}

func startFixtureServer(t *testing.T, bin, root string) *fixtureServer {
	t.Helper()

	cmd := exec.Command(bin, "--root", root)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	f := &fixtureServer{
		stdout: bufio.NewReader(stdout),
		stdin:  stdin,
		cmd:    cmd,
		stderr: stderr,
	}
	t.Cleanup(func() {
		if f.cmd.ProcessState == nil {
			f.cmd.Process.Kill()
			f.cmd.Wait()
		}
	})

	line, err := f.stdout.ReadString('\n')
	if err != nil {
		t.Fatalf("reading the READY line: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.HasPrefix(line, "READY ") {
		t.Fatalf("first stdout line = %q, want a READY line\nstderr:\n%s", line, stderr)
	}

	var ready readyLine
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "READY "))), &ready); err != nil {
		t.Fatalf("READY line is not JSON: %v\n%s", err, line)
	}
	if ready.Host != "htmlclay" {
		t.Errorf("READY host = %q, want htmlclay", ready.Host)
	}
	if want := "/_/clayjs/" + clayruntime.Selected + "/"; ready.Runtime != want {
		t.Errorf("READY runtime = %q, want %q", ready.Runtime, want)
	}
	if !strings.HasPrefix(ready.BaseURL, "http://127.0.0.1:") {
		t.Errorf("READY baseURL = %q, want a 127.0.0.1 URL", ready.BaseURL)
	}
	if _, port, err := net.SplitHostPort(strings.TrimPrefix(ready.BaseURL, "http://")); err != nil || port == "" {
		t.Errorf("READY baseURL has no port: %q", ready.BaseURL)
	}

	f.baseURL = ready.BaseURL
	f.runtime = ready.Runtime
	f.documents = ready.Documents
	return f
}

func (f *fixtureServer) get(t *testing.T, target string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading GET %s: %v", target, err)
	}
	return resp, body
}

// documentETag reads the root element's stamp the way a client does, by parsing
// the response rather than by searching its bytes.
func documentETag(t *testing.T, body []byte) string {
	t.Helper()
	z := html.NewTokenizer(bytes.NewReader(body))
	for {
		switch z.Next() {
		case html.ErrorToken:
			t.Fatalf("no root element in %q", body)
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			if !strings.EqualFold(tok.Data, "html") {
				continue
			}
			for _, a := range tok.Attr {
				if strings.EqualFold(a.Key, "documentetag") {
					return a.Val
				}
			}
			return ""
		}
	}
}

func TestOfflineFixtureServerServesTheContract(t *testing.T) {
	bin := buildFixtureServer(t)
	root := writeFixtures(t)
	f := startFixtureServer(t, bin, root)

	t.Run("every embedded runtime file", func(t *testing.T) {
		files := clayruntime.Releases[clayruntime.Selected].Files
		if len(files) == 0 {
			t.Fatal("the selected release lists no files")
		}
		counted := 0
		for name, entry := range files {
			resp, body := f.get(t, f.baseURL+f.runtime+name)
			if resp.StatusCode != 200 {
				t.Errorf("%s = %d", name, resp.StatusCode)
				continue
			}
			sum := sha256.Sum256(body)
			if got := hex.EncodeToString(sum[:]); got != entry.SHA256 {
				t.Errorf("%s bytes hash %s, want %s", name, got, entry.SHA256)
			}
			counted++
		}
		if counted != len(files) {
			t.Fatalf("served %d of %d files", counted, len(files))
		}
	})

	t.Run("served documents", func(t *testing.T) {
		for _, rel := range []string{"board.html", "notes/2026/doc.html"} {
			if _, ok := f.documents[rel]; !ok {
				t.Fatalf("READY lists no %q: %v", rel, f.documents)
			}
		}

		_, served := f.get(t, f.documents["board.html"].URL)
		for _, want := range []string{
			f.runtime + "clay.js?plugins=sortable,people,upload,sync,visitor-drafts,unsent,menu&exclude=richclay",
			f.runtime + "sap.js",
			f.runtime + "src/page/themes/light-1.css",
			`id="clay-runtime-map"`,
		} {
			if !strings.Contains(string(served), want) {
				t.Errorf("board.html is missing %q:\n%s", want, served)
			}
		}
		if strings.Contains(string(served), `src="https://clayjs.com/v1/`) {
			t.Errorf("board.html still loads the public runtime:\n%s", served)
		}

		_, plain := f.get(t, f.documents["notes/2026/doc.html"].URL)
		for _, unwanted := range []string{"/_/clayjs/", "clay-runtime-map"} {
			if strings.Contains(string(plain), unwanted) {
				t.Errorf("notes/2026/doc.html was projected (%q):\n%s", unwanted, plain)
			}
		}
		if !strings.Contains(string(plain), `<p>note</p>`) {
			t.Errorf("notes/2026/doc.html lost its body:\n%s", plain)
		}
	})

	t.Run("save", func(t *testing.T) {
		_, served := f.get(t, f.documents["board.html"].URL)
		etag := documentETag(t, served)
		if etag == "" {
			t.Fatal("the served board.html carries no documentetag")
		}
		edited := strings.Replace(string(served), "<p>board</p>", "<p>board edited</p>", 1)
		if edited == string(served) {
			t.Fatal("the served board.html has no board text node to edit")
		}

		save := f.documents["board.html"].Save
		if save.Method != "POST" {
			t.Errorf("save method = %q, want POST", save.Method)
		}
		req, err := http.NewRequest(save.Method, save.URL, strings.NewReader(edited))
		if err != nil {
			t.Fatal(err)
		}
		for name, value := range save.Headers {
			req.Header.Set(name, value)
		}
		req.Header.Set("If-Match", etag)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("save = %d %s", resp.StatusCode, body)
		}

		disk, err := os.ReadFile(filepath.Join(root, "board.html"))
		if err != nil {
			t.Fatal(err)
		}
		for _, unwanted := range []string{"/_/clayjs/", "clay-runtime-map"} {
			if strings.Contains(string(disk), unwanted) {
				t.Errorf("the saved document kept %q on disk:\n%s", unwanted, disk)
			}
		}
		for _, want := range []string{
			`src="https://clayjs.com/v1/clay.js?plugins=sortable,people,upload,sync,visitor-drafts,unsent,menu&exclude=richclay"`,
			`src="https://clayjs.com/v1/sap.js"`,
			`href="https://clayjs.com/v1/src/page/themes/light-1.css"`,
			"board edited",
		} {
			if !strings.Contains(string(disk), want) {
				t.Errorf("the saved document lost %q:\n%s", want, disk)
			}
		}
	})

	t.Run("sigterm", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows cannot deliver SIGTERM; the graceful exit is checked on macOS and Linux")
		}
		addr := strings.TrimPrefix(f.baseURL, "http://")
		if err := f.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		exited := make(chan error, 1)
		go func() { exited <- f.cmd.Wait() }()
		select {
		case err := <-exited:
			if err != nil {
				t.Fatalf("exit after SIGTERM: %v\nstderr:\n%s", err, f.stderr)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("still running 10s after SIGTERM\nstderr:\n%s", f.stderr)
		}
		if rest, _ := io.ReadAll(f.stdout); len(rest) != 0 {
			t.Errorf("stdout carried more than the READY line: %q", rest)
		}
		if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			conn.Close()
			t.Errorf("the port %s still accepts connections after shutdown", addr)
		}
	})
}
