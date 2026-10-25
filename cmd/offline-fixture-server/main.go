// Command offline-fixture-server serves a folder of fixture documents through
// HTML Clay's real server, so another host's contract test can fetch and save
// them without a browser, a tray or any of the user's own HTML Clay state.
//
//	offline-fixture-server --root DIR
//
// It binds 127.0.0.1 on an ephemeral port, prints exactly one READY line on
// stdout, logs to stderr, and keeps serving until SIGTERM or stdin EOF, then
// shuts down and exits 0.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/panphora/htmlclay/internal/clayruntime"
	"github.com/panphora/htmlclay/internal/logging"
	"github.com/panphora/htmlclay/internal/server"
	"github.com/panphora/htmlclay/internal/session"
	"github.com/panphora/htmlclay/internal/versions"
)

// saveTarget is how a caller writes a document back: the body is the served
// (projected) HTML, plus only If-Match.
type saveTarget struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

type document struct {
	URL  string     `json:"url"`
	Save saveTarget `json:"save"`
}

type readyLine struct {
	Host      string              `json:"host"`
	BaseURL   string              `json:"baseURL"`
	Runtime   string              `json:"runtime"`
	Documents map[string]document `json:"documents"`
}

func main() {
	root := flag.String("root", "", "directory of fixture documents to serve")
	flag.Parse()
	if err := run(*root, os.Stdout, os.Stdin); err != nil {
		fmt.Fprintf(os.Stderr, "offline-fixture-server: %v\n", err)
		os.Exit(1)
	}
}

func run(root string, stdout io.Writer, stdin io.Reader) error {
	if root == "" {
		return errors.New("--root is required")
	}
	resolved, err := resolveRoot(root)
	if err != nil {
		return err
	}

	// The server reads the user's home for the uploads library and the session
	// manager holds a home root. Point home at a throwaway directory and hand
	// the session manager the served folder itself, so this process cannot see
	// or write the user's real HTML Clay config, history or uploads.
	home, err := os.MkdirTemp("", "htmlclay-offline-fixture-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	if err := os.Setenv("HOME", home); err != nil {
		return err
	}
	if err := os.Setenv("USERPROFILE", home); err != nil {
		return err
	}

	mgr := session.NewManagerWithHome(resolved)
	defer mgr.RevokeAll()

	logger, err := logging.NewDualWrite(filepath.Join(home, "server.log"))
	if err != nil {
		return err
	}
	defer logger.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()

	srv := server.New(ln, mgr, logger, versions.New(filepath.Join(home, "versions")))
	baseURL := "http://" + srv.Host()

	paths, err := fixtureDocuments(resolved)
	if err != nil {
		return err
	}
	documents := make(map[string]document, len(paths))
	for _, path := range paths {
		f, err := mgr.Register(path, session.ViaOsOpen)
		if err != nil {
			return fmt.Errorf("register %s: %w", path, err)
		}
		rel, err := filepath.Rel(resolved, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		documents[rel] = document{
			URL: baseURL + "/" + rel,
			Save: saveTarget{
				Method: "POST",
				URL:    baseURL + "/_/save/" + f.Token,
				Headers: map[string]string{
					"Content-Type":   "text/plain;charset=UTF-8",
					"Sec-Fetch-Site": "same-origin",
					"Origin":         baseURL,
				},
			},
		}
	}

	served := make(chan error, 1)
	go func() { served <- srv.Start() }()

	line, err := json.Marshal(readyLine{
		Host:      "htmlclay",
		BaseURL:   baseURL,
		Runtime:   clayruntime.LocalPrefix + clayruntime.Selected + "/",
		Documents: documents,
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "READY %s\n", line); err != nil {
		return err
	}
	logger.Printf("Serving %d documents from %s at %s", len(documents), resolved, baseURL)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(signals)

	stdinClosed := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, stdin)
		close(stdinClosed)
	}()

	select {
	case sig := <-signals:
		logger.Printf("Received %s, shutting down", sig)
	case <-stdinClosed:
		logger.Printf("stdin closed, shutting down")
	case err := <-served:
		if err != nil {
			return err
		}
		return errors.New("server stopped")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		return err
	}
	if err := <-served; err != nil {
		return err
	}
	return nil
}

func resolveRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("--root %q is not a directory", root)
	}
	return resolved, nil
}

// fixtureDocuments lists every document under root, in a stable order.
func fixtureDocuments(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".html", ".htmlclay":
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}
