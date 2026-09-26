package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"softafrique-backup-agent/internal/config"
	resticpkg "softafrique-backup-agent/internal/restic"
	"softafrique-backup-agent/internal/scheduler"
	"softafrique-backup-agent/internal/status"
)

// Agent ties together configuration, restic and status reporting.
type Agent struct {
	cfg   *config.Config
	rest  *resticpkg.Restic
	store *status.Store
	log   *slog.Logger
	// Version is the agent build version reported in the status file. Injected
	// by the CLI (main.Version).
	Version string
}

// New creates an Agent.
func New(cfg *config.Config, store *status.Store, log *slog.Logger) *Agent {
	return &Agent{
		cfg:   cfg,
		rest:  resticpkg.New(cfg),
		store: store,
		log:   log,
	}
}

// PingSelfTest verifies the repo is reachable and the credentials work.
func (a *Agent) PingSelfTest(ctx context.Context) error {
	if err := a.rest.Ping(ctx); err != nil {
		return fmt.Errorf("repo self-test failed: %w", err)
	}
	return nil
}

// SelfTest initializes the repo if needed, then verifies connectivity.
func (a *Agent) SelfTest(ctx context.Context) error {
	if err := a.rest.EnsureRepo(ctx); err != nil {
		return err
	}
	return a.PingSelfTest(ctx)
}

// RunBackup performs a single backup now and records the outcome in status.
// It returns the snapshot id on success.
func (a *Agent) RunBackup(ctx context.Context) (string, error) {
	a.log.Info("starting backup", "paths", a.cfg.Include)
	if err := a.rest.EnsureRepo(ctx); err != nil {
		return "", err
	}
	res, err := a.rest.Backup(ctx, a.cfg.Include, a.cfg.Exclude)
	if err != nil {
		return "", fmt.Errorf("backup failed: %w", err)
	}

	st, err := a.store.Load()
	if err != nil {
		return "", fmt.Errorf("read status before save: %w", err)
	}
	st.LastBackupTime = time.Now().UTC().Format(time.RFC3339)
	st.LastBackupStatus = "success"
	st.LastBackupError = ""
	st.AgentVersion = a.Version
	st.LastSnapshotID = res.SnapshotID
	st.LastDurationSeconds = res.TotalDuration.Seconds()
	st.FilesAdded = res.FilesAdded
	st.FilesChanged = res.FilesChanged
	st.BytesAdded = res.BytesAdded
	st.BackupCount++
	if a.cfg.ScheduleInterval > 0 {
		st.NextBackupTime = time.Now().Add(a.cfg.ScheduleInterval).UTC().Format(time.RFC3339)
	}
	if err := a.store.Save(st); err != nil {
		a.log.Warn("could not persist status", "err", err)
	}

	a.log.Info("backup complete",
		"snapshot", res.SnapshotID,
		"files_added", res.FilesAdded,
		"files_changed", res.FilesChanged,
		"bytes_added", res.BytesAdded,
		"duration", res.TotalDuration.Round(time.Second),
	)
	return res.SnapshotID, nil
}

// RunScheduled starts the scheduled + retry loop and blocks until ctx dies.
func (a *Agent) RunScheduled(ctx context.Context) error {
	sched := scheduler.New(a.cfg, a.store, a.RunBackupAsFunc, a.log)
	return sched.Run(ctx)
}

// RunBackupAsFunc adapts RunBackup to the scheduler's error-only signature.
func (a *Agent) RunBackupAsFunc(ctx context.Context) error {
	_, err := a.RunBackup(ctx)
	return err
}

// Restore snapshot ("latest" allowed) into target, optional include filters.
func (a *Agent) Restore(ctx context.Context, snapshot, target string, include []string) error {
	a.log.Info("restoring snapshot", "snapshot", snapshot, "target", target, "include", include)
	return a.rest.Restore(ctx, snapshot, target, include)
}

// LatestSnapshot returns the most recent snapshot metadata (nil when repo is
// new/empty).
func (a *Agent) LatestSnapshot(ctx context.Context) (*resticpkg.Snapshot, error) {
	return a.rest.LatestSnapshot(ctx)
}