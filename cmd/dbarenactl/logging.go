package main

import (
	"log/slog"
	"os"
	"path/filepath"
)

// initLogging points the default slog logger at dbarenactl's diagnostic log
// file (INFO+), appended across invocations. Callers must close the
// returned file once the command finishes running.
func initLogging() (*os.File, error) {
	path, err := logFilePath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})))
	return f, nil
}
