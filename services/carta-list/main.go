package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	CtlAddress string
	SiteID     string
	SessionID  string
	User       string
	Token      string
	BaseFolder string
	ConfigPath string
	LogLevel   string
	LogDir     string
	AlsoStdout bool
}

type Service struct {
	cfg Config
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
	body := strings.NewReader(fmt.Sprintf(`{"sessionId":"%s","siteId":"%s","user":"%s","token":"%s"}`,
		s.cfg.SessionID, s.cfg.SiteID, s.cfg.User, s.cfg.Token,
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

	slog.Info("registered with carta-ctl", "url", registerURL, "status", resp.Status)
	return nil
}
