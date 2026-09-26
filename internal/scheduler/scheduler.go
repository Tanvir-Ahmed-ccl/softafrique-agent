package scheduler

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	"softafrique-backup-agent/internal/config"
	"softafrique-backup-agent/internal/status"
)

// BackupFunc performs a single backup. Implementations must return nil on
// success or an error on failure. It is safe to run one at a time.
type BackupFunc func(ctx context.Context) error

// Scheduler triggers backups on a schedule and retries failed runs when the
// endpoint was unreachable (e.g. the PC was offline).
type Scheduler struct {
	cfg   *config.Config
	store *status.Store
	run   BackupFunc
	log   *slog.Logger
}

// New creates a Scheduler.
func New(cfg *config.Config, store *status.Store, run BackupFunc, log *slog.Logger) *Scheduler {
	return &Scheduler{cfg: cfg, store: store, run: run, log: log}
}

// Run loops until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	for {
		st, err := s.store.Load()
		if err != nil {
			s.log.Warn("could not read status file", "err", err)
		}

		if wait := s.waitUntilDue(ctx, st); wait > 0 {
			s.log.Info("next backup not due", "wait", wait.Round(time.Second))
			if !sleepCtx(ctx, wait) {
				return nil
			}
		}

		if err := s.runWithRetry(ctx); err != nil {
			s.log.Error("backup failed after retries", "err", err)
		}

		if ctx.Err() != nil {
			return nil
		}
	}
}

// waitUntilDue computes how long to sleep before the next backup. A fresh
// install with no prior snapshot backs up immediately.
func (s *Scheduler) waitUntilDue(ctx context.Context, st *status.Status) time.Duration {
	if st == nil || st.LastBackupTime == "" || st.LastBackupStatus != "success" {
		return 0
	}
	last, err := time.Parse(time.RFC3339, st.LastBackupTime)
	if err != nil {
		return 0
	}
	due := last.Add(s.cfg.ScheduleInterval)
	remaining := time.Until(due)
	if remaining <= 0 {
		return 0
	}
	return remaining
}

// runWithRetry executes one backup, retrying with the configured interval on
// failure. Offline PCs keep retrying until success or ctx cancellation.
func (s *Scheduler) runWithRetry(ctx context.Context) error {
	jitter := time.Duration(0)
	if s.cfg.Jitter > 0 {
		jitter = time.Duration(rand.Int64N(int64(s.cfg.Jitter)))
	}
	if jitter > 0 {
		s.log.Info("adding startup jitter to spread server load", "jitter", jitter.Round(time.Second))
		if !sleepCtx(ctx, jitter) {
			return ctx.Err()
		}
	}

	attempts := 0
	for {
		attempts++
		markRunning(s.store, s.cfg)

		err := s.run(ctx)

		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}

		if s.cfg.MaxRetries >= 0 && attempts > s.cfg.MaxRetries {
			s.log.Error("giving up on backup", "attempts", attempts)
			markFailed(s.store, err.Error(), s.cfg)
			return err
		}

		// Backoff caps at 5x the base interval to avoid hammering an
		// unreachable server forever at high frequency.
		backoff := s.cfg.RetryInterval * time.Duration(attempts)
		if backoff > s.cfg.RetryInterval*5 {
			backoff = s.cfg.RetryInterval * 5
		}
		s.log.Warn("backup failed, will retry", "attempts", attempts, "retry_in", backoff.Round(time.Second), "err", err)
		if !sleepCtx(ctx, backoff) {
			return err
		}
	}
}

func markRunning(store *status.Store, cfg *config.Config) {
	st, err := store.Load()
	if err != nil {
		st = &status.Status{}
	}
	st.LastBackupStatus = "running"
	st.LastRunStart = time.Now().UTC().Format(time.RFC3339)
	st.DeviceID = cfg.DeviceID
	_ = store.Save(st)
}

func markFailed(store *status.Store, errMsg string, cfg *config.Config) {
	st, err := store.Load()
	if err != nil {
		st = &status.Status{}
	}
	st.LastBackupError = errMsg
	st.DeviceID = cfg.DeviceID
	_ = store.Save(st)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}