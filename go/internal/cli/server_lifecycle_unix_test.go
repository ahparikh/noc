//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// nocCommand runs this test binary as the noc executable.
func nocCommand(args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "NOC_CLI_TEST_AS_NOC=1")
	return cmd
}

func runNoc(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	cmd := nocCommand(args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = exitErr.ExitCode()
	}
	return code, stdout.String(), stderr.String()
}

func TestBackgroundServerHelper(t *testing.T) {
	// TestMain turns the test binary into noc; prove the wiring works.
	isolate(t)
	if code, stdout, _ := runNoc(t, "version"); code != 0 || stdout != "dev\n" {
		t.Fatalf("helper version = %d %q", code, stdout)
	}
	if code, _, stderr := runNoc(t, "bogus"); code != 2 || !strings.Contains(stderr, `unknown command "bogus"`) {
		t.Fatalf("helper bogus = %d %q", code, stderr)
	}
}

func readRecord(t *testing.T, path string) serverPIDRecord {
	t.Helper()
	record, err := readServerPIDFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestBackgroundServerLifecycle(t *testing.T) {
	dir := isolate(t)
	run := filepath.Join(dir, "run")
	pidFile := filepath.Join(run, "noc.pid")
	t.Cleanup(func() { runNoc(t, "server", "stop") })

	code, stdout, stderr := runNoc(t, "server", "--bg", "--discovery=false")
	if code != 0 || stderr != "" {
		t.Fatalf("--bg = %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	record := readRecord(t, pidFile)
	wantOut := fmt.Sprintf("noc server started in background (pid %d)\nlogs: %s\n", record.PID, filepath.Join(run, "noc.log"))
	if stdout != wantOut {
		t.Fatalf("stdout = %q, want %q", stdout, wantOut)
	}
	if record.PID <= 1 || record.Mode != "background" || record.SocketPath != filepath.Join(run, "noc.sock") {
		t.Fatalf("record = %+v", record)
	}
	raw, _ := os.ReadFile(pidFile)
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 4 || !bytes.HasSuffix(raw, []byte("}\n")) {
		t.Fatalf("raw record = %q (%v)", raw, err)
	}
	for _, path := range []string{pidFile, filepath.Join(run, "noc.log"), filepath.Join(dir, "state", "noc.db")} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s = %v %v", path, info, err)
		}
	}
	for _, path := range []string{run, filepath.Join(dir, "state")} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s = %v %v", path, info, err)
		}
	}

	// Duplicate start is rejected without disturbing the server.
	code, _, stderr = runNoc(t, "server", "--bg", "--discovery=false")
	if code != 1 || stderr != "noc: noc server is already running\nStop the previous instance by running: noc server stop\n" {
		t.Fatalf("duplicate = %d %q", code, stderr)
	}
	code, _, stderr = runNoc(t, "server", "--discovery=false")
	if code != 1 || !strings.Contains(stderr, "noc server is already running") {
		t.Fatalf("foreground duplicate = %d %q", code, stderr)
	}
	if again := readRecord(t, pidFile); again.PID != record.PID {
		t.Fatalf("record changed: %+v", again)
	}

	code, stdout, _ = runNoc(t, "server", "status")
	if code != 0 || !strings.HasPrefix(stdout, "noc server ok (dev)\n") ||
		!strings.Contains(stdout, fmt.Sprintf("  process: pid %d (background)\n", record.PID)) ||
		!strings.Contains(stdout, "claude: disabled") || !strings.Contains(stdout, "codex: disabled") || strings.Contains(stdout, "hint:") {
		t.Fatalf("status = %d\n%s", code, stdout)
	}

	code, stdout, stderr = runNoc(t, "server", "restart", "--discovery=false", "--timeout=3s")
	if code != 0 {
		t.Fatalf("restart = %d %q %q", code, stdout, stderr)
	}
	restarted := readRecord(t, pidFile)
	if restarted.PID == record.PID || restarted.Mode != "background" {
		t.Fatalf("restarted record = %+v", restarted)
	}
	wantOut = fmt.Sprintf("noc server stopped (pid %d)\nnoc server restarted in background (pid %d)\nlogs: %s\n",
		record.PID, restarted.PID, filepath.Join(run, "noc.log"))
	if stdout != wantOut {
		t.Fatalf("restart stdout = %q, want %q", stdout, wantOut)
	}

	code, stdout, stderr = runNoc(t, "server", "stop")
	if code != 0 || stdout != fmt.Sprintf("noc server stopped (pid %d)\n", restarted.PID) {
		t.Fatalf("stop = %d %q %q", code, stdout, stderr)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("pid file still present: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(run, "noc.sock")); !os.IsNotExist(err) {
		t.Fatalf("socket still present: %v", err)
	}
	entries, _ := os.ReadDir(run)
	if len(entries) != 1 || entries[0].Name() != "noc.log" {
		t.Fatalf("run dir = %v", entries)
	}
	code, _, stderr = runNoc(t, "server", "stop")
	if code != 1 || stderr != "noc: noc server is not running\n" {
		t.Fatalf("second stop = %d %q", code, stderr)
	}
	code, _, stderr = runNoc(t, "server", "restart", "--discovery=false")
	if code != 1 || stderr != "noc: noc server is not running\n" {
		t.Fatalf("restart while stopped = %d %q", code, stderr)
	}
}

// startForegroundChild runs a foreground server as a separate process.
func startForegroundChild(t *testing.T) (*exec.Cmd, string) {
	t.Helper()
	dir := isolate(t)
	cmd := nocCommand("server", "--discovery=false")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	pidFile := filepath.Join(dir, "run", "noc.pid")
	waitFor(t, "foreground server", func() bool {
		code, _, _ := runCLI(t, "server", "status", "--json")
		return code == 0
	})
	return cmd, pidFile
}

func TestServerRestartRejectsForegroundServer(t *testing.T) {
	cmd, pidFile := startForegroundChild(t)
	if record := readRecord(t, pidFile); record.Mode != "foreground" || record.PID != cmd.Process.Pid {
		t.Fatalf("record = %+v", record)
	}
	code, stdout, stderr := runCLI(t, "server", "restart", "--discovery=false")
	if code != 1 || stdout != "" || stderr != "noc: noc server is running in the foreground; stop it with Ctrl-C\n" {
		t.Fatalf("restart = %d %q %q", code, stdout, stderr)
	}
	if code, _, _ := runCLI(t, "server", "status", "--json"); code != 0 {
		t.Fatal("foreground server should keep running")
	}
}

func TestServerStopStopsManagedServer(t *testing.T) {
	cmd, pidFile := startForegroundChild(t)
	pid := cmd.Process.Pid
	code, stdout, stderr := runCLI(t, "server", "stop", "--timeout=5s")
	if code != 0 || stdout != fmt.Sprintf("noc server stopped (pid %d)\n", pid) || stderr != "" {
		t.Fatalf("stop = %d %q %q", code, stdout, stderr)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server process did not exit")
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("pid file: %v", err)
	}
}

func TestPIDRecordEdgeCases(t *testing.T) {
	dir := isolate(t)
	run := filepath.Join(dir, "run")
	if err := os.MkdirAll(run, 0o700); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(run, "noc.pid")
	sock := filepath.Join(run, "noc.sock")

	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(pidFile, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("not json")
	code, _, stderr := runCLI(t, "server", "--bg", "--discovery=false")
	if code != 1 || !strings.Contains(stderr, "noc: decode server PID file "+fmt.Sprintf("%q", pidFile)) {
		t.Fatalf("corrupt bg = %d %q", code, stderr)
	}
	if code, _, stderr := runCLI(t, "server", "stop"); code != 1 || !strings.Contains(stderr, "decode server PID file") {
		t.Fatalf("corrupt stop = %d %q", code, stderr)
	}
	write(`{"pid":999999,"socket_path":"` + sock + `","mode":"weird"}`)
	if code, _, stderr := runCLI(t, "server", "stop"); code != 1 || !strings.Contains(stderr, `has invalid mode "weird"`) {
		t.Fatalf("mode = %d %q", code, stderr)
	}
	write(`{"pid":1,"socket_path":"` + sock + `"}`)
	if _, err := readServerPIDFile(pidFile); err == nil || !strings.Contains(err.Error(), "is invalid") {
		t.Fatalf("pid 1 err = %v", err)
	}
	write(strings.Repeat("x", maxPIDFileBytes+1))
	if _, err := readServerPIDFile(pidFile); err == nil || !strings.Contains(err.Error(), "exceeds 16384 bytes") {
		t.Fatalf("oversize err = %v", err)
	}
	write(`{"pid":999999,"socket_path":"` + sock + `","mode":"background"}`)
	if code, _, stderr := runCLI(t, "server", "stop"); code != 1 || stderr != "noc: noc server is not running\n" {
		t.Fatalf("dead stop = %d %q", code, stderr)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("stale record should be removed: %v", err)
	}
	write(`{"pid":999999,"socket_path":"/elsewhere.sock","mode":"background"}`)
	if code, _, stderr := runCLI(t, "server", "stop"); code != 1 || !strings.Contains(stderr, `server PID file targets socket "/elsewhere.sock"`) {
		t.Fatalf("mismatch = %d %q", code, stderr)
	}

	// A stale record is replaced during acquisition; a live owner is not.
	write(`{"pid":999999,"socket_path":"` + sock + `"}`)
	release, err := acquireServerPIDFile(pidFile, sock, "foreground")
	if err != nil {
		t.Fatal(err)
	}
	if record := readRecord(t, pidFile); record.PID != os.Getpid() || record.Mode != "foreground" {
		t.Fatalf("record = %+v", record)
	}
	if _, err := acquireServerPIDFile(pidFile, sock, "foreground"); !isServerAlreadyRunningError(err) {
		t.Fatalf("second acquire err = %v", err)
	}
	if err := removeOwnedPIDFile(pidFile, os.Getpid()+1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatalf("foreign owner removed record: %v", err)
	}
	release()
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("release left record: %v", err)
	}

	if alive, err := processIsAlive(os.Getpid()); err != nil || !alive {
		t.Fatalf("self alive = %v %v", alive, err)
	}
	if alive, err := processIsAlive(999999); err != nil || alive {
		t.Fatalf("dead alive = %v %v", alive, err)
	}
	if _, err := processIsAlive(1); err == nil {
		t.Fatal("pid 1 should be invalid")
	}
}

func TestStartupNotifierProtocol(t *testing.T) {
	isolate(t)
	read := func(setup func(n *startupNotifier)) string {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		n := &startupNotifier{file: w}
		setup(n)
		n.close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		_ = r.Close()
		return buf.String()
	}
	if got := read(func(n *startupNotifier) { n.ready(); n.fail(fmt.Errorf("late")) }); got != "ready\n" {
		t.Fatalf("ready = %q", got)
	}
	if got := read(func(n *startupNotifier) { n.fail(fmt.Errorf("%w (pid 4)", errServerAlreadyRunning)) }); got != "already-running\n" {
		t.Fatalf("already = %q", got)
	}
	if got := read(func(n *startupNotifier) { n.fail(fmt.Errorf("a\nb")) }); got != "error:a b\n" {
		t.Fatalf("error = %q", got)
	}
	if got := read(func(n *startupNotifier) {}); got != "" {
		t.Fatalf("silent = %q", got)
	}
	var nilNotifier *startupNotifier
	nilNotifier.ready()
	nilNotifier.close()
	t.Setenv(readyFDEnvironment, "2")
	if startupNotifierFromEnvironment() != nil {
		t.Fatal("fd < 3 must be ignored")
	}
	if _, ok := os.LookupEnv(readyFDEnvironment); ok {
		t.Fatal("variable must be unset")
	}
	_ = context.Background
}
