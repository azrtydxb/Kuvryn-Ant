// Command qafixture starts or stops the real-backend browser QA harness:
// a disposable socket-only PostgreSQL, a bootstrapped token in a mode-0600
// credential file, and a loopback app. Usage: go run ./cmd/qafixture start|stop
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/localfixture"
)

type state struct {
	Root string `json:"root"`
	PID  int    `json:"pid"`
	Base string `json:"base"`
}

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "start" && os.Args[1] != "stop") {
		fmt.Fprintln(os.Stderr, "usage: qafixture start|stop")
		os.Exit(2)
	}
	repo, err := repoRoot()
	if err == nil {
		statePath := filepath.Join(repo, "web", ".qa-fixture.json")
		if os.Args[1] == "stop" {
			err = stop(statePath)
		} else {
			err = start(repo, statePath)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func repoRoot() (string, error) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		return "", fmt.Errorf("run from inside the repository: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func stop(statePath string) error {
	raw, err := os.ReadFile(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var info state
	if err = json.Unmarshal(raw, &info); err != nil {
		return err
	}
	_ = syscall.Kill(info.PID, syscall.SIGTERM)
	for i := 0; i < 50; i++ {
		if syscall.Kill(info.PID, 0) != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	localfixture.StopPostgres(info.Root)
	_ = os.RemoveAll(info.Root)
	_ = os.Remove(statePath)
	fmt.Println("Fixture stopped and private data removed.")
	return nil
}

func start(repo, statePath string) (err error) {
	if _, e := os.Stat(statePath); e == nil {
		return errors.New("fixture already exists; stop it first")
	}
	syscall.Umask(0o077)
	f, err := localfixture.New(repo, "ant-console-", "ant_console")
	if err != nil {
		return err
	}
	var srv *localfixture.Server
	defer func() {
		if err != nil {
			if srv != nil {
				srv.Kill()
			}
			f.Cleanup()
		}
	}()
	if err = f.StartPostgres(); err != nil {
		return err
	}
	if err = f.Build(); err != nil {
		return err
	}
	credential, err := f.Bootstrap("console-qa")
	if err != nil {
		return err
	}
	if !json.Valid(credential) {
		return errors.New("bootstrap did not return JSON")
	}
	if err = os.WriteFile(f.Path("credential.json"), credential, 0o600); err != nil {
		return err
	}
	port, err := localfixture.FreePort()
	if err != nil {
		return err
	}
	if srv, err = f.StartServer(port, f.Path("app.log"), true); err != nil {
		return err
	}
	pid := srv.Cmd.Process.Pid
	raw, _ := json.Marshal(state{Root: f.Root, PID: pid, Base: srv.Base})
	if err = os.WriteFile(statePath, raw, 0o644); err != nil {
		return err
	}
	out, _ := json.Marshal(map[string]any{"base": srv.Base, "credential_file": f.Path("credential.json"), "pid": pid})
	fmt.Println(string(out))
	return nil
}
