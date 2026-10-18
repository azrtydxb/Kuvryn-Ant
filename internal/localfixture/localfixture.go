// Package localfixture provisions a disposable, socket-only PostgreSQL cluster
// and a real Ant server for local smoke and QA tooling. It needs the PostgreSQL
// binaries (initdb, pg_ctl, createdb) and Go on PATH. No mock API is involved.
package localfixture

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Fixture is a private temporary directory holding the cluster, root key and binary.
type Fixture struct {
	Root     string
	RepoDir  string
	Env      []string
	database string
}

// New creates the private directory and root key. database names the database.
func New(repoDir, prefix, database string) (*Fixture, error) {
	for _, tool := range []string{"initdb", "pg_ctl", "createdb", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, fmt.Errorf("required tool missing: %s", tool)
		}
	}
	// /tmp keeps the Unix socket path short enough on every platform.
	root, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		return nil, err
	}
	f := &Fixture{Root: root, RepoDir: repoDir, database: database}
	key := make([]byte, 32)
	if _, err = rand.Read(key); err == nil {
		err = os.WriteFile(f.Path("root.key"), key, 0o600)
	}
	if err != nil {
		os.RemoveAll(root)
		return nil, err
	}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "ANT_") {
			f.Env = append(f.Env, kv)
		}
	}
	f.Env = append(f.Env,
		fmt.Sprintf("ANT_DATABASE_URL=host=%s user=ant dbname=%s sslmode=disable", root, database),
		"ANT_ROOT_KEY_FILE="+f.Path("root.key"),
		"ANT_DATA_DIR="+f.Path("storage"),
		"ANT_STORAGE_BACKEND=disk")
	return f, nil
}

// Path joins elements under the fixture root.
func (f *Fixture) Path(elem ...string) string {
	return filepath.Join(append([]string{f.Root}, elem...)...)
}

// SetEnv sets or replaces an environment variable for later commands.
func (f *Fixture) SetEnv(key, value string) { f.Env = WithEnv(f.Env, key, value) }

// WithEnv returns env with key set to value.
func WithEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+value)
}

// Run executes a command in the repository directory. Failures never include
// child output because tools may print credentials; go build stderr is the exception.
func (f *Fixture) Run(args ...string) ([]byte, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env, cmd.Dir = f.Env, f.RepoDir
	var stderr strings.Builder
	if args[0] == "go" {
		cmd.Stderr = &stderr
	}
	out, err := cmd.Output()
	if err != nil {
		if stderr.Len() > 0 {
			fmt.Fprintln(os.Stderr, stderr.String())
		}
		return nil, fmt.Errorf("%s failed; private fixture cleanup follows", filepath.Base(args[0]))
	}
	return out, nil
}

// StartPostgres initializes and starts a socket-only cluster and creates the database.
func (f *Fixture) StartPostgres() error {
	steps := [][]string{
		{"initdb", "-D", f.Path("pg"), "-A", "trust", "-U", "ant", "--no-locale"},
		{"pg_ctl", "-D", f.Path("pg"), "-l", f.Path("pg.log"), "-o", "-k " + f.Root + " -h ''", "-w", "start"},
		{"createdb", "-h", f.Root, "-U", "ant", f.database},
	}
	for _, s := range steps {
		if _, err := f.Run(s...); err != nil {
			return err
		}
	}
	return nil
}

// StopPostgres stops the cluster rooted at root; errors are ignored (cleanup).
func StopPostgres(root string) {
	_ = exec.Command("pg_ctl", "-D", filepath.Join(root, "pg"), "-m", "immediate", "stop").Run()
}

// Build compiles ./cmd/ant into the fixture directory.
func (f *Fixture) Build() error {
	_, err := f.Run("go", "build", "-o", f.Path("ant"), "./cmd/ant")
	return err
}

// Bootstrap runs `ant bootstrap name` and returns the raw credential JSON.
func (f *Fixture) Bootstrap(name string) ([]byte, error) {
	return f.Run(f.Path("ant"), "bootstrap", name)
}

// FreePort returns an unused loopback TCP port.
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// Server is a running app process.
type Server struct {
	Cmd  *exec.Cmd
	Base string
	done chan struct{}
}

// StartServer launches the built binary on port, appending output to logPath,
// and waits for readiness. newSession detaches it from the caller's session.
func (f *Fixture) StartServer(port int, logPath string, newSession bool) (*Server, error) {
	f.SetEnv("ANT_LISTEN", fmt.Sprintf("127.0.0.1:%d", port))
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	cmd := exec.Command(f.Path("ant"))
	cmd.Env, cmd.Stdout, cmd.Stderr = f.Env, log, log
	if newSession {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	s := &Server{Cmd: cmd, Base: fmt.Sprintf("http://127.0.0.1:%d", port), done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(s.done) }()
	if err = s.WaitReady(15 * time.Second); err != nil {
		s.Kill()
		return nil, err
	}
	return s, nil
}

// WaitReady polls /health/ready until success, process exit or timeout.
func (s *Server) WaitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		resp, err := http.Get(s.Base + "/health/ready")
		if err == nil {
			resp.Body.Close()
			return nil
		}
		select {
		case <-s.done:
			return errors.New("app exited; inspect private app log")
		default:
		}
		if time.Now().After(deadline) {
			return errors.New("app did not become ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Terminate sends SIGTERM and waits up to 20s, then kills.
func (s *Server) Terminate(ctx context.Context) {
	_ = s.Cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(20 * time.Second):
		s.Kill()
	case <-ctx.Done():
		s.Kill()
	}
}

// Kill force-stops the process and waits for it.
func (s *Server) Kill() {
	_ = s.Cmd.Process.Kill()
	<-s.done
}

// Cleanup stops PostgreSQL and removes all private data.
func (f *Fixture) Cleanup() {
	StopPostgres(f.Root)
	os.RemoveAll(f.Root)
}
