package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	Execute()
}

func configureLogger(level, logFile string) {
	opts := &slog.HandlerOptions{Level: parseLogLevel(level)}
	var w io.Writer = os.Stderr

	if logFile != "" {
		if dir := filepath.Dir(logFile); dir != "." {
			_ = os.MkdirAll(dir, 0755) // best-effort; failure is caught by OpenFile below
		}
		f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, opts)))
			slog.Warn("cannot open log file, falling back to stderr only",
				"path", logFile, "error", err)
		} else {
			w = io.MultiWriter(os.Stderr, f)
		}
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(w, opts)))
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "trace":
		return slog.Level(-8)
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "fatal":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
