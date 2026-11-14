package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"noc.local/noc/internal/config"
	"noc.local/noc/internal/localapi"
)

const (
	maxPIDFileBytes        = 16384
	backgroundStartTimeout = 45 * time.Second
	serverProbeTimeout     = 300 * time.Millisecond
	stopPollInterval       = 50 * time.Millisecond
	readyFDEnvironment     = "NOC_INTERNAL_SERVER_READY_FD"

	pidModeForeground = "foreground"
	pidModeBackground = "background"
)

var errServerAlreadyRunning = errors.New("noc server is already running")

// serverPIDRecord is the JSON document stored in the PID file.
type serverPIDRecord struct {
	PID        int       `json:"pid"`
	SocketPath string    `json:"socket_path"`
	StartedAt  time.Time `json:"started_at"`
	Mode       string    `json:"mode,omitempty"`
}

// serverOptions are the launch values forwarded to a background child.
type serverOptions struct {
	Socket            string
	Database          string
	PIDFile           string
	LogFormat         string
	Discovery         bool
	DiscoveryInterval time.Duration
	StaleAfter        time.Duration
}

func defaultServerPIDFile(socketPath string) string {
	return filepath.Join(filepath.Dir(socketPath), "noc.pid")
}

func defaultServerLogFile(pidFile string) string {
	return filepath.Join(filepath.Dir(pidFile), "noc.log")
}

// acquireServerPIDFile atomically claims the PID record and returns a release
// function that removes it if it is still owned by this process.
func acquireServerPIDFile(path, socketPath, mode string) (func(), error) {
	if err := config.EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			record := serverPIDRecord{
				PID:        os.Getpid(),
				SocketPath: filepath.Clean(socketPath),
				StartedAt:  time.Now().UTC(),
				Mode:       mode,
			}
			if err := json.NewEncoder(file).Encode(record); err != nil {
				_ = file.Close()
				_ = os.Remove(path)
				return nil, fmt.Errorf("write server PID file %q: %w", path, err)
			}
			if err := file.Close(); err != nil {
				_ = os.Remove(path)
				return nil, fmt.Errorf("close server PID file %q: %w", path, err)
			}
			pid := record.PID
			return func() { _ = removeOwnedPIDFile(path, pid) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create server PID file %q: %w", path, err)
		}
		existing, err := readServerPIDFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		alive, err := processIsAlive(existing.PID)
		if err != nil {
			return nil, err
		}
		if alive {
			return nil, fmt.Errorf("%w (pid %d)", errServerAlreadyRunning, existing.PID)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("remove stale server PID file %q: %w", path, err)
		}
	}
	return nil, fmt.Errorf("acquire server PID file %q after concurrent updates", path)
}

// readServerPIDFile reads and validates the record. An open failure is
// returned unwrapped so callers can test os.ErrNotExist.
func readServerPIDFile(path string) (serverPIDRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		return serverPIDRecord{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPIDFileBytes+1))
	if err != nil {
		return serverPIDRecord{}, fmt.Errorf("read server PID file %q: %w", path, err)
	}
	if len(data) > maxPIDFileBytes {
		return serverPIDRecord{}, fmt.Errorf("server PID file %q exceeds %d bytes", path, maxPIDFileBytes)
	}
	var record serverPIDRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return serverPIDRecord{}, fmt.Errorf("decode server PID file %q: %w", path, err)
	}
	if record.PID <= 1 || strings.TrimSpace(record.SocketPath) == "" {
		return serverPIDRecord{}, fmt.Errorf("server PID file %q is invalid", path)
	}
	switch record.Mode {
	case "", pidModeForeground, pidModeBackground:
	default:
		return serverPIDRecord{}, fmt.Errorf("server PID file %q has invalid mode %q", path, record.Mode)
	}
	record.SocketPath = filepath.Clean(record.SocketPath)
	return record, nil
}

// removeOwnedPIDFile removes the record only while it still names pid.
func removeOwnedPIDFile(path string, pid int) error {
	record, err := readServerPIDFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.PID != pid {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// checkServerAlreadyRunning is the background pre-check: PID record first,
// then a short health probe on the socket.
func checkServerAlreadyRunning(ctx context.Context, socketPath, pidFile string) error {
	record, err := readServerPIDFile(pidFile)
	switch {
	case err == nil:
		alive, err := processIsAlive(record.PID)
		if err != nil {
			return err
		}
		if alive {
			return fmt.Errorf("%w (pid %d)", errServerAlreadyRunning, record.PID)
		}
		if err := os.Remove(pidFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale server PID file %q: %w", pidFile, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if serverSocketReachable(ctx, socketPath) {
		return errServerAlreadyRunning
	}
	return nil
}

func serverSocketReachable(ctx context.Context, socketPath string) bool {
	ctx, cancel := context.WithTimeout(ctx, serverProbeTimeout)
	defer cancel()
	client := localapi.NewClient(socketPath)
	defer client.Close()
	_, err := client.Health(ctx)
	return err == nil
}

// startBackgroundServer spawns a detached server and waits for its readiness
// report. It returns the child's PID and the log path.
func startBackgroundServer(ctx context.Context, opts serverOptions) (int, string, error) {
	executable, err := os.Executable()
	if err != nil {
		return 0, "", fmt.Errorf("resolve noc executable: %w", err)
	}
	logPath := defaultServerLogFile(opts.PIDFile)
	if err := config.EnsurePrivateDir(filepath.Dir(logPath)); err != nil {
		return 0, "", err
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return 0, "", fmt.Errorf("open background server log %q: %w", logPath, err)
	}
	defer logFile.Close()
	if err := os.Chmod(logPath, 0o600); err != nil {
		return 0, "", fmt.Errorf("secure background server log %q: %w", logPath, err)
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return 0, "", fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer devNull.Close()
	reader, writer, err := os.Pipe()
	if err != nil {
		return 0, "", fmt.Errorf("create readiness pipe: %w", err)
	}
	defer reader.Close()

	cmd := exec.Command(executable, backgroundServerArgs(opts)...)
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.ExtraFiles = []*os.File{writer}
	cmd.Env = replaceEnvironmentValue(os.Environ(), readyFDEnvironment, "3")
	if err := configureBackgroundCommand(cmd); err != nil {
		_ = writer.Close()
		return 0, "", err
	}
	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		return 0, "", fmt.Errorf("start background noc server: %w", err)
	}
	_ = writer.Close()
	pid := cmd.Process.Pid

	lines := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(reader).ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			line = "closed"
		}
		lines <- line
	}()

	timer := time.NewTimer(backgroundStartTimeout)
	defer timer.Stop()
	terminate := func() {
		_ = signalProcessTermination(pid)
		_, _ = cmd.Process.Wait()
	}
	select {
	case line := <-lines:
		releaseErr := cmd.Process.Release()
		switch {
		case line == "ready":
			if releaseErr != nil {
				return 0, "", fmt.Errorf("release background server process: %w", releaseErr)
			}
			return pid, logPath, nil
		case line == "already-running":
			return 0, "", errServerAlreadyRunning
		case strings.HasPrefix(line, "error:"):
			return 0, "", fmt.Errorf("background server failed to start: %s", strings.TrimSpace(strings.TrimPrefix(line, "error:")))
		default:
			return 0, "", fmt.Errorf("background server exited before becoming ready; see %s", logPath)
		}
	case <-timer.C:
		terminate()
		return 0, "", fmt.Errorf("background server did not become ready within %s; see %s", backgroundStartTimeout, logPath)
	case <-ctx.Done():
		terminate()
		return 0, "", ctx.Err()
	}
}

func backgroundServerArgs(opts serverOptions) []string {
	return []string{
		"server",
		"--socket=" + opts.Socket,
		"--database=" + opts.Database,
		"--pid-file=" + opts.PIDFile,
		"--log-format=" + opts.LogFormat,
		"--discovery=" + strconv.FormatBool(opts.Discovery),
		"--discovery-interval=" + opts.DiscoveryInterval.String(),
		"--stale-after=" + opts.StaleAfter.String(),
	}
}

// replaceEnvironmentValue returns env with key set to value, dropping any
// existing entries for key.
func replaceEnvironmentValue(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}

// startupNotifier reports startup progress to a background parent.
type startupNotifier struct {
	file *os.File
	once sync.Once
}

// startupNotifierFromEnvironment returns nil for a foreground server.
func startupNotifierFromEnvironment() *startupNotifier {
	value := os.Getenv(readyFDEnvironment)
	if value == "" {
		return nil
	}
	_ = os.Unsetenv(readyFDEnvironment)
	fd, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || fd < 3 {
		return nil
	}
	return &startupNotifier{file: os.NewFile(uintptr(fd), "noc-ready")}
}

func (n *startupNotifier) send(line string) {
	if n == nil {
		return
	}
	n.once.Do(func() {
		_, _ = io.WriteString(n.file, line+"\n")
		_ = n.file.Close()
	})
}

func (n *startupNotifier) ready() { n.send("ready") }

// fail reports a startup failure; it is a no-op once ready was sent.
func (n *startupNotifier) fail(err error) {
	if n == nil {
		return
	}
	if isServerAlreadyRunningError(err) {
		n.send("already-running")
		return
	}
	n.send("error:" + strings.ReplaceAll(err.Error(), "\n", " "))
}

// close releases the descriptor if nothing was ever sent.
func (n *startupNotifier) close() {
	if n == nil {
		return
	}
	n.once.Do(func() { _ = n.file.Close() })
}

func isServerAlreadyRunningError(err error) bool {
	return errors.Is(err, errServerAlreadyRunning) || errors.Is(err, localapi.ErrServerRunning)
}

// requireRestartableServer refuses to restart anything but a background server.
func requireRestartableServer(pidFile string) error {
	record, err := readServerPIDFile(pidFile)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("noc server is not running")
	}
	if err != nil {
		return err
	}
	if record.Mode == pidModeForeground {
		return errors.New("noc server is running in the foreground; stop it with Ctrl-C")
	}
	return nil
}

// stopServerProcess SIGTERMs the managed server and waits for it to go away.
func stopServerProcess(ctx context.Context, socketPath, pidFile string, timeout time.Duration) (int, error) {
	record, err := readServerPIDFile(pidFile)
	if errors.Is(err, os.ErrNotExist) {
		if serverSocketReachable(ctx, socketPath) {
			return 0, fmt.Errorf("server is reachable at %q but PID file %q is missing", socketPath, pidFile)
		}
		return 0, errors.New("noc server is not running")
	}
	if err != nil {
		return 0, err
	}
	if filepath.Clean(socketPath) != record.SocketPath {
		return 0, fmt.Errorf("server PID file targets socket %q, not %q", record.SocketPath, filepath.Clean(socketPath))
	}
	alive, err := processIsAlive(record.PID)
	if err != nil {
		return 0, err
	}
	if !alive {
		_ = removeOwnedPIDFile(pidFile, record.PID)
		return 0, errors.New("noc server is not running")
	}
	if err := signalProcessTermination(record.PID); err != nil {
		return 0, fmt.Errorf("stop noc server process %d: %w", record.PID, err)
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(stopPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-deadline.C:
			return 0, fmt.Errorf("server process %d did not stop within %s", record.PID, timeout)
		case <-ticker.C:
		}
		_, pidErr := os.Stat(pidFile)
		_, sockErr := os.Lstat(socketPath)
		if errors.Is(pidErr, os.ErrNotExist) && errors.Is(sockErr, os.ErrNotExist) {
			return record.PID, nil
		}
		if alive, err := processIsAlive(record.PID); err == nil && !alive {
			_ = removeOwnedPIDFile(pidFile, record.PID)
			return record.PID, nil
		}
	}
}
