package processHelpers

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/gorilla/websocket"

	helpers "github.com/CARTAvis/go-carta/pkg/shared"
)

// package-scope regex and parser for worker readiness log lines
var listenRe = regexp.MustCompile(`Listening on port (\d+) with top level folder`)

func parsePortFromLine(line string) (int, bool) {
	m := listenRe.FindStringSubmatch(line)
	if len(m) != 2 {
		return 0, false
	}
	p, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return p, true
}

// SpawnWorker starts a new worker process and waits until the worker logs that
// it is listening ("server listening at ..."). The worker is started with
// -port=0 so the OS selects a free port, and the detected port from the log is
// returned.
func SpawnWorker(ctx context.Context, workerPath string, timeoutDuration time.Duration, username string, baseDirTmpl string, topLevelDir string) (*exec.Cmd, int, error) {
	user, err := user.Lookup(username)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to lookup user %s: %w", username, err)
	}

	args := []string{"--debug_no_auth"}
	args = append(args, "--no_frontend")
	args = append(args, "--no_database")
	args = append(args, "--controller_deployment")
	args = append(args, "--verbosity", "5")
	args = append(args, "--exit_timeout", "10")
	args = append(args, "--initial_timeout", "20")
	args = append(args, "--idle_timeout", "300")
	if topLevelDir != "" {
		args = append(args, "--top_level_folder", topLevelDir)
	}

	// Adding as a positional argument so startup folder should be last option
	if strings.Contains(baseDirTmpl, "{{.home}}") && user.HomeDir == "" {
		slog.Warn("base_dir_tmpl references {{.home}} but user has no home directory. Omitting starting directory", "username", username)
	} else if baseDirTmpl != "" {
		var buf bytes.Buffer
		tmpl, err := template.New("base_dir_tmpl").Parse(baseDirTmpl)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to parse base_dir_tmpl: %w", err)
		}

		err = tmpl.Execute(&buf, map[string]string{
			"user": username,
			"home": user.HomeDir,
		})
		if err != nil {
			return nil, 0, fmt.Errorf("failed to execute base_dir_tmpl: %w", err)
		}
		resolvedDir := buf.String()
		slog.Debug("Resolved base directory from template", "template", baseDirTmpl, "resolved", resolvedDir)
		info, err := os.Stat(resolvedDir)
		if err != nil {
			slog.Error("Failed to stat resolved base directory. Omitting it.", "directory", resolvedDir, "error", err)
		} else if !info.IsDir() {
			slog.Warn("Resolved base directory is not a directory. Omitting it.", "directory", resolvedDir)
		} else {
			args = append(args, resolvedDir)
		}
	}

	slog.Info("Spawning worker process", "workerPath", workerPath, "username", username, "args", args)

	cmd := exec.CommandContext(ctx, "sudo", append([]string{"-u", username, workerPath}, args...)...)

	// Capture stdout/stderr so we can watch for the readiness log while still
	// forwarding output to the parent process' stdio.
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	// Start the worker process.
	if err := cmd.Start(); err != nil {
		return nil, 0, fmt.Errorf("failed to start worker: %w", err)
	}

	// Channel to signal readiness once the expected log line is observed
	// (carries the detected port).
	readyCh := make(chan int, 1)

	slog.Debug("Worker process started, waiting for readiness")

	// TODO: I need to go over this code a bit more
	// Helper to scan a pipe, forward lines, and watch for readiness.
	watch := func(r io.Reader, w io.Writer) {
		s := bufio.NewScanner(r)
		for s.Scan() {
			line := s.Text()
			// Forward the line to the appropriate writer.
			_, err := fmt.Fprintln(w, line)
			if err != nil {
				return
			}
			// Detect readiness: parse port from log line.
			slog.Debug("Scanning line for port info", "line", line)
			if p, ok := parsePortFromLine(line); ok {
				slog.Info("Detected worker port from log", "port", p)
				// Send detected port if not already sent.
				select {
				case readyCh <- p:
				default:
				}
			}
			slog.Debug("Finished scanning line", "line", line)
		}
	}

	slog.Debug("Starting to watch worker stdout/stderr for readiness")

	// Start scanning goroutines.
	go watch(stdoutPipe, os.Stdout)
	go watch(stderrPipe, os.Stderr)

	slog.Debug("Watching worker output for readiness")

	// Wait for readiness or timeout; kill the worker on failure.
	ctxReady, cancel := context.WithTimeout(ctx, timeoutDuration)
	defer cancel()
	select {
	case p := <-readyCh:
		return cmd, p, nil
	case <-ctxReady.Done():
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, 0, fmt.Errorf("worker did not become ready in time: %w", ctxReady.Err())
	}
}



func resolveExecutablePath(executablePath string) (string, error) {
	if executablePath == "" {
		return "", fmt.Errorf("empty executable path")
	}
	if filepath.IsAbs(executablePath) || strings.ContainsRune(executablePath, os.PathSeparator) {
		return executablePath, nil
	}
	if resolved, err := exec.LookPath(executablePath); err == nil {
		return resolved, nil
	}
	self, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(self), executablePath)
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("unable to resolve executable %q via PATH or relative to current binary", executablePath)
}

func TestWorker(ctx context.Context, port int, timeoutDuration time.Duration) error {
	addr := fmt.Sprintf("ws://localhost:%d", port)

	rpcCtx, cancel := context.WithTimeout(ctx, timeoutDuration)
	defer cancel()
	// Connect to the worker websocket
	conn, _, err := websocket.DefaultDialer.DialContext(rpcCtx, addr, nil)
	if err != nil {
		return err
	}
	defer helpers.CloseOrLog(conn)

	// Send a PING text message and wait for a PONG
	err = conn.WriteMessage(websocket.TextMessage, []byte("PING"))
	if err != nil {
		return err
	}
	messageType, message, err := conn.ReadMessage()
	if err != nil {
		return err
	}
	if messageType != websocket.TextMessage {
		return fmt.Errorf("expected text message, got %d", messageType)
	}
	if string(message) != "PONG" {
		return fmt.Errorf("expected PONG, got %s", string(message))
	}

	return nil
}


func SpawnCartaList(ctx context.Context, executablePath string, username string, ctlAddress string, sessionID string, siteID string, token string, baseFolder string) (*exec.Cmd, error) {
	resolvedExec, err := resolveExecutablePath(executablePath)
	if err != nil {
		return nil, err
	}
	args := []string{
		"--ctl-address", ctlAddress,
		"--session-id", sessionID,
		"--site-id", siteID,
		"--token", token,
	}
	effectiveUser := username
	if effectiveUser != "" && effectiveUser != "anonymous" {
		args = append(args, "--user", effectiveUser)
	}
	if baseFolder != "" {
		args = append(args, "--base-folder", baseFolder)
	}

	slog.Info("Spawning carta-list process", "configuredPath", executablePath, "resolvedPath", resolvedExec, "requestedUsername", username, "effectiveUser", effectiveUser, "args", args)

	var cmd *exec.Cmd
	if effectiveUser == "" || effectiveUser == "anonymous" {
		slog.Warn("Launching carta-list as current spawner user because requested user is empty or anonymous", "requestedUsername", username)
		cmd = exec.CommandContext(ctx, resolvedExec, args...)
	} else {
		cmd = exec.CommandContext(ctx, "sudo", append([]string{"-u", effectiveUser, resolvedExec}, args...)...)
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start carta-list with executable %q for requested user %q (effective user %q): %w", resolvedExec, username, effectiveUser, err)
	}
	return cmd, nil
}
