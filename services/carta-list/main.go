package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	CtlAddress  string
	SiteID      string
	SessionID   string
	User        string
	Token       string
	BaseFolder  string
	ConfigPath  string
	LogLevel    string
	LogDir      string
	AlsoStdout  bool
	WorkerExec  string
	TopLevelDir string
}

type Service struct {
	cfg Config
	// backendAddress is the host:port of the carta_backend instance this
	// carta-list spawned to answer listing messages. It is reported to
	// carta-ctl during registration so the controller can proxy file-list
	// traffic to it. Interim measure until carta-list does listing natively.
	backendAddress string
}

func main() {
	cfg := parseFlags()
	logger, closeFn, err := setupLogger("carta-list", cfg)
	if err != nil {
		log.Fatalf("failed to set up logger: %v", err)
	}
	defer closeFn()
	slog.SetDefault(logger)

	slog.Info("starting carta-list",
		"user", cfg.User,
		"site", cfg.SiteID,
		"session", cfg.SessionID,
		"ctl", cfg.CtlAddress,
		"base", cfg.BaseFolder,
		"config", cfg.ConfigPath,
	)

	svc := &Service{cfg: cfg}
	ctx := context.Background()
	if err := svc.Run(ctx); err != nil {
		slog.Error("carta-list exited with error", "error", err)
		os.Exit(1)
	}
}

func parseFlags() Config {
	cfg := Config{}
	flag.StringVar(&cfg.ConfigPath, "config", "/etc/carta/config.toml", "Path to config file")
	flag.StringVar(&cfg.CtlAddress, "ctl-address", "http://127.0.0.1:8081", "address of local carta-ctl to connect back to")
	flag.StringVar(&cfg.SiteID, "site-id", "home", "logical site identifier")
	flag.StringVar(&cfg.SessionID, "session-id", "", "frontend or control session identifier")
	flag.StringVar(&cfg.User, "user", "", "authenticated local username")
	flag.StringVar(&cfg.Token, "token", "", "short-lived control token issued by carta-ctl")
	flag.StringVar(&cfg.BaseFolder, "base-folder", ".", "root folder exposed for listing")
	flag.Parse()

	applyConfigFile(&cfg)

	if cfg.User == "" {
		cfg.User = os.Getenv("USER")
	}
	if cfg.BaseFolder == "" {
		cfg.BaseFolder = "."
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.LogDir == "" {
		cfg.LogDir = "./logs"
	}
	if cfg.WorkerExec == "" {
		cfg.WorkerExec = "carta_backend"
	}
	return cfg
}

func applyConfigFile(cfg *Config) {
	v := viper.New()
	v.SetConfigFile(cfg.ConfigPath)
	v.SetConfigType("toml")
	v.SetDefault("log_level", "info")
	v.SetDefault("logging.dir", "./logs")
	v.SetDefault("logging.also_stdout", true)

	if err := v.ReadInConfig(); err != nil {
		// Missing config is fine for dev and direct spawning.
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok && !strings.Contains(strings.ToLower(err.Error()), "no such file") {
			fmt.Fprintf(os.Stderr, "warning: failed to read config %q: %v\n", cfg.ConfigPath, err)
		}
	} else {
		cfg.LogLevel = v.GetString("log_level")
		cfg.LogDir = v.GetString("logging.dir")
		cfg.AlsoStdout = v.GetBool("logging.also_stdout")
		cfg.WorkerExec = v.GetString("spawner.worker_exec")
		cfg.TopLevelDir = v.GetString("spawner.top_level_dir")
	}
}

func setupLogger(service string, cfg Config) (*slog.Logger, func(), error) {
	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("create log dir %q: %w", cfg.LogDir, err)
	}
	logPath := filepath.Join(cfg.LogDir, service+".log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file %q: %w", logPath, err)
	}

	var out io.Writer = f
	if cfg.AlsoStdout {
		out = io.MultiWriter(os.Stdout, f)
	}
	level := parseLevel(cfg.LogLevel)
	handler := slog.NewTextHandler(out, &slog.HandlerOptions{Level: level})
	return slog.New(handler), func() { _ = f.Close() }, nil
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func (s *Service) Run(ctx context.Context) error {
	base, err := filepath.Abs(s.cfg.BaseFolder)
	if err != nil {
		return fmt.Errorf("resolve base folder: %w", err)
	}
	slog.Info("resolved base folder", "base", base)

	// Interim listing strategy: spawn a carta_backend instance (as this same
	// user) and let it answer the file-listing protocol. carta-ctl proxies
	// FILE_LIST_REQUEST traffic to the address we report below.
	backendCmd, err := s.startListingBackend(ctx, base)
	if err != nil {
		return fmt.Errorf("start listing backend: %w", err)
	}
	defer func() {
		if backendCmd != nil && backendCmd.Process != nil {
			_ = backendCmd.Process.Kill()
		}
	}()

	if err := s.registerWithCtl(ctx); err != nil {
		return err
	}

	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("shutdown requested")
			return nil
		case <-heartbeat.C:
			slog.Info("heartbeat", "site", s.cfg.SiteID, "user", s.cfg.User, "session", s.cfg.SessionID)
		}
	}
}

func (s *Service) registerWithCtl(ctx context.Context) error {
	_ = ctx

	registerURL := strings.TrimRight(s.cfg.CtlAddress, "/") + "/api/internal/carta-list/register"
	body := strings.NewReader(fmt.Sprintf(`{"sessionId":"%s","siteId":"%s","user":"%s","token":"%s","backendAddress":"%s"}`,
		s.cfg.SessionID, s.cfg.SiteID, s.cfg.User, s.cfg.Token, s.backendAddress,
	))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registerURL, body)
	if err != nil {
		return fmt.Errorf("build registration request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("register with carta-ctl: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("register with carta-ctl returned status %s", resp.Status)
	}

	slog.Info("registered with carta-ctl", "url", registerURL, "status", resp.Status, "backendAddress", s.backendAddress)
	return nil
}

// backendListenRe matches the carta_backend readiness log line and captures the
// port it bound to.
var backendListenRe = regexp.MustCompile(`Listening on port (\d+)`)

// startListingBackend launches a carta_backend instance to serve the file
// listing protocol and waits until it reports the port it is listening on. It
// stores the resulting host:port in s.backendAddress and returns the running
// command so the caller can shut it down on exit.
func (s *Service) startListingBackend(ctx context.Context, baseDir string) (*exec.Cmd, error) {
	exe := s.cfg.WorkerExec
	if exe == "" {
		exe = "carta_backend"
	}

	// These flags mirror the worker spawn in carta-spawn: no frontend, no
	// database, driven entirely over the controller's websocket. Listing-
	// specific backend flags belong here, in the carta-list wrapper.
	args := []string{
		"--debug_no_auth",
		"--no_frontend",
		"--no_database",
		"--controller_deployment",
		"--verbosity", "5",
		"--exit_timeout", "10",
		"--initial_timeout", "20",
		"--idle_timeout", "300",
	}
	if s.cfg.TopLevelDir != "" {
		args = append(args, "--top_level_folder", s.cfg.TopLevelDir)
	}
	if baseDir != "" {
		// Positional starting directory must be last.
		args = append(args, baseDir)
	}

	slog.Info("starting listing backend", "exe", exe, "args", args)
	cmd := exec.CommandContext(ctx, exe, args...)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("backend stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("backend stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start backend: %w", err)
	}

	portCh := make(chan int, 1)
	watch := func(r io.Reader, w io.Writer) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			fmt.Fprintln(w, line)
			if m := backendListenRe.FindStringSubmatch(line); len(m) == 2 {
				if p, perr := strconv.Atoi(m[1]); perr == nil {
					select {
					case portCh <- p:
					default:
					}
				}
			}
		}
	}
	go watch(stdoutPipe, os.Stdout)
	go watch(stderrPipe, os.Stderr)

	select {
	case p := <-portCh:
		s.backendAddress = fmt.Sprintf("127.0.0.1:%d", p)
		slog.Info("listing backend ready", "address", s.backendAddress)
		return cmd, nil
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("listing backend did not report a listening port in time")
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return nil, ctx.Err()
	}
}
