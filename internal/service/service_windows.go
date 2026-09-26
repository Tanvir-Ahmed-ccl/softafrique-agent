//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// Name is the Windows service name registered in the SCM.
const Name = "SoftafriqueBackupAgent"

// DisplayName shown in the Services console.
const DisplayName = "Softafrique Backup Agent"

// Description shown in the Services console.
const Description = "Backs up customer folders securely to backup.softafrique.net using Restic."

type handler struct {
	run func(ctx context.Context) error
	log *slog.Logger
}

// Execute implements svc.Handler.
func (h *handler) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown

	var elog *eventlog.Log
	if el, err := eventlog.Open(Name); err == nil {
		elog = el
		defer elog.Close()
	} else {
		h.log.Warn("could not open event log", "err", err)
	}
	announce := func(level string, msg string) {
		h.log.Info(msg)
		if elog != nil {
			switch level {
			case "error":
				_ = elog.Error(1, msg)
			case "warning":
				_ = elog.Warning(1, msg)
			default:
				_ = elog.Info(1, msg)
			}
		}
	}

	announce("info", "Softafrique Backup Agent service starting")

	changes <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()

	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				announce("info", "Softafrique Backup Agent service stopping")
				cancel()
				changes <- svc.Status{State: svc.StopPending}
				// Give the loop a moment to unwind: cancelling the context
				// stops the current restic run, and the agent still has a
				// status file to write on the way out. Exiting the handler
				// immediately would let the SCM kill the process mid-write.
				select {
				case err := <-done:
					if err != nil && !errors.Is(err, context.Canceled) {
						announce("warning", fmt.Sprintf("backup loop stopped with: %v", err))
					}
				case <-time.After(30 * time.Second):
					announce("warning", "backup loop did not stop within 30s; exiting anyway")
				}
				return false, 0
			}
		case err := <-done:
			if err != nil {
				announce("error", fmt.Sprintf("backup loop exited with error: %v", err))
				changes <- svc.Status{State: svc.StopPending}
				return true, 1
			}
			announce("info", "backup loop exited cleanly")
			changes <- svc.Status{State: svc.StopPending}
			return false, 0
		}
	}
}

// RunService registers the svc handler and blocks until the service stops.
func RunService(run func(ctx context.Context) error, log *slog.Logger) error {
	return svc.Run(Name, &handler{run: run, log: log})
}

// InstallService creates the Windows service registered to run this exe.
func InstallService(exePath string) error {
	mgrCtl, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer mgrCtl.Disconnect()

	s, err := mgrCtl.CreateService(
		Name,
		exePath,
		mgr.Config{
			DisplayName: DisplayName,
			Description: Description,
			StartType:   mgr.StartAutomatic,
			// LocalSystem can read files and run backups even when nobody is
			// signed in.
			ServiceStartName: "LocalSystem",
		},
		"service",
	)
	if err != nil {
		return fmt.Errorf("CreateService: %w", err)
	}
	defer s.Close()

	if err := eventlog.InstallAsEventCreate(
		Name,
		eventlog.Error|eventlog.Warning|eventlog.Info,
	); err != nil {
		return fmt.Errorf("eventlog install: %w", err)
	}
	return nil
}

// UninstallService removes the Windows service.
func UninstallService() error {
	mgrCtl, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer mgrCtl.Disconnect()

	s, err := mgrCtl.OpenService(Name)
	if err != nil {
		return fmt.Errorf("OpenService: %w", err)
	}
	defer s.Close()

	if err := s.Delete(); err != nil {
		return fmt.Errorf("Delete: %w", err)
	}
	_ = eventlog.Remove(Name)
	return nil
}
