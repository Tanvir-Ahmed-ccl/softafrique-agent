//go:build !windows

package service

import (
	"context"
	"errors"
	"log/slog"
)

// Name is kept identical across platforms so command output stays consistent.
const Name = "SoftafriqueBackupAgent"

// DisplayName for reference.
const DisplayName = "Softafrique Backup Agent"

// Description for reference.
const Description = "Backs up customer folders securely to backup.softafrique.net using Restic."

// RunService is unsupported on non-Windows hosts; run the agent in console mode
// instead (`agent run`).
func RunService(_ func(ctx context.Context) error, _ *slog.Logger) error {
	return errors.New("Windows service mode is only supported on Windows; use 'run' for console mode")
}

// InstallService is unsupported on non-Windows hosts.
func InstallService(_ string) error {
	return errors.New("service installation is only supported on Windows")
}

// UninstallService is unsupported on non-Windows hosts.
func UninstallService() error {
	return errors.New("service removal is only supported on Windows")
}