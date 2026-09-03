package main

import (
	"os"
	"path/filepath"
)

// dbarenactlHome returns the directory dbarenactl keeps all of its local state in:
// the SQLite database, per-sweep lock files, and fetched run artifacts.
// Overridable via DBARENACTL_HOME for tests and for running multiple isolated
// instances on one machine.
func dbarenactlHome() (string, error) {
	if v := os.Getenv("DBARENACTL_HOME"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".dbarenactl"), nil
}

func dbPath() (string, error) {
	home, err := dbarenactlHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "dbarenactl.db"), nil
}

func lockPath(sweepID string) (string, error) {
	home, err := dbarenactlHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "sweeps", sweepID+".lock"), nil
}

func artifactBaseDir(sweepID string) (string, error) {
	home, err := dbarenactlHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "sweeps", sweepID, "runs"), nil
}

// logsBaseDir is where per-run benchctl output logs live -- one file per
// run id, named <runID>.log, written by internal/bench.Client.
func logsBaseDir(sweepID string) (string, error) {
	home, err := dbarenactlHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "sweeps", sweepID, "logs"), nil
}

// logFilePath is dbarenactl's own diagnostic log (INFO+), separate from the
// per-run benchctl output logs under logsBaseDir.
func logFilePath() (string, error) {
	home, err := dbarenactlHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "dbarenactl.log"), nil
}
