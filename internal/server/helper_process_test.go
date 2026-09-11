package server

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as the registered program. The dispatcher starts a
// program with no arguments and passes this process's environment through, so
// the behaviour is chosen by environment variable, the way os/exec's own tests
// re-run themselves.
const (
	serverHelperMode = "HTMLCLAY_SERVER_HELPER_MODE"
	serverHelperArg  = "HTMLCLAY_SERVER_HELPER_ARG"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(serverHelperMode); mode != "" {
		runServerHelperProcess(mode, os.Getenv(serverHelperArg))
		return
	}
	os.Exit(m.Run())
}

func structuredHelper(t *testing.T, mode string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(serverHelperMode, mode)
	return exe
}

func runServerHelperProcess(mode, arg string) {
	switch mode {
	case "result-null":
		fmt.Println(`{"type":"result","value":null}`)
	case "count":
		request, _ := io.ReadAll(os.Stdin)
		if !strings.Contains(string(request), `"helperProtocol":1`) {
			fmt.Println(`{"type":"error","code":"unstamped","message":"missing helper protocol"}`)
			return
		}
		count := 0
		if data, err := os.ReadFile(arg); err == nil {
			count, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		count++
		os.WriteFile(arg, []byte(strconv.Itoa(count)+"\n"), 0644)
		cwd, _ := os.Getwd()
		json.NewEncoder(os.Stdout).Encode(map[string]any{
			"type":  "result",
			"value": map[string]any{"count": count, "cwd": cwd},
		})
	case "hold":
		fmt.Println(`{"type":"status","text":"started"}`)
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		<-stop
	case "release":
		fmt.Println(`{"type":"status","text":"running"}`)
		for {
			if _, err := os.Stat(arg); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		fmt.Println(`{"type":"result","value":{"real":true}}`)
	case "document-mode":
		request, _ := io.ReadAll(os.Stdin)
		if strings.Contains(string(request), `"document":"none"`) {
			fmt.Println(`{"type":"result","value":null}`)
		} else {
			fmt.Println(`{"type":"error","code":"unstamped","message":"the request carried no resolved document mode"}`)
		}
	}
}
