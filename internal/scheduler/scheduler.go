// Package scheduler decides when a backup runs, and retries it when it fails.
//
// The schedule is derived from the last success persisted in status.json rather
// than from an in-memory timer, so a reboot, a service restart or a crash
// resumes the correct schedule instead of starting the clock again.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"softafrique-backup-agent/internal/config"
	"softafrique-backup-agent/internal/status"
)

// ErrTerminal wraps a failure that must not be retried.
//
// The scheduler returns immediately on it. A device whose credentials the
// gateway has revoked is the case that matters: retrying every few minutes
// against a server that will keep saying no is pointless traffic, and a
// suspended device should sit quietly until an operator re-enrolls it.
var ErrTerminal = errors.New("not retryable")

// RunFunc performs one backup attempt and describes the outcome. It returns an
// error for logging and retry purposes; the status.Outcome carries the details
// that get recorded.
type RunFunc func(ctx context.Context) (status.Outcome, error)

// Scheduler triggers backups on a schedule and retries failed runs.
type Scheduler struct {
	cfg   *config.Config
	store *status.Store
	run   RunFunc
	log   *slog.Logger

	// now and sleep are injectable so the timing rules can be tested without
	// waiting minutes for them.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) bool
}

// New creates a Scheduler.
func New(cfg *config.Config, store *status.Store, run RunFunc, log *slog.Logger) *Scheduler {
	return &Scheduler{
		cfg:   cfg,
		store: store,
		run:   run,
		log:   log,
		now:   time.Now,
		sleep: sleepCtx,
	}
}

// Run loops until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	for {
		st, err := s.store.Load()
		if err != nil {
			// A status file that cannot be read is not a reason to stop backing
			// up; the worst case is that the schedule restarts.
			s.log.Warn("could not read status file", "err", err)
		}
		if st == nil {
			st = status.New()
		}

		if wait := s.waitUntilDue(st); wait > 0 {
			s.log.Info("next backup not due", "wait", wait.Round(time.Second), "next", st.NextBackupTime)
			if !s.sleep(ctx, wait) {
				return nil
			}
		}

		if err := s.runWithRetry(ctx); err != nil && !errors.Is(err, context.Canceled) {
			if errors.Is(err, ErrTerminal) {
				s.log.Warn("attempt will not be retried", "err", err)
			} else {
				s.log.Error("backup failed after retries", "err", err)
			}
		}

		if ctx.Err() != nil {
			return nil
		}
	}
}

// RunOnce performs a single attempt with no retry and records it.
//
// It backs the `agent backup` command, which an operator or an RMM script runs
// by hand. Routing the one-shot command through here rather than calling the run
// function directly is deliberate: this is the only place an attempt is
// recorded, so a backup started by hand lands in status.json exactly like one
// the schedule started, and there is no second code path that can forget.
func (s *Scheduler) RunOnce(ctx context.Context) (status.Outcome, error) {
	return s.attempt(ctx)
}

// waitUntilDue computes how long to sleep before the next attempt.
//
// A fresh install, or one that has never succeeded, runs immediately. A device
// the gateway has suspended waits a retry interval rather than trying again at
// full speed, because nothing will change until someone acts.
func (s *Scheduler) waitUntilDue(st *status.Status) time.Duration {
	if st.LastAttemptStatus == status.AttemptSuspended {
		return s.cfg.RetryInterval
	}
	if st.LastSuccess == "" {
		return 0
	}
	last, ok := status.ParseStamp(st.LastSuccess)
	if !ok {
		return 0
	}
	remaining := last.Add(s.cfg.ScheduleInterval).Sub(s.now())
	if remaining <= 0 {
		return 0
	}
	return remaining
}

// runWithRetry performs an attempt and retries it on failure.
//
// Retries carry jitter as well as the startup delay does. Without it, every
// agent that was switched off during a gateway outage wakes up, backs off by
// the same amount, and retries in the same instant, which is how a recovering
// gateway gets knocked over again.
func (s *Scheduler) runWithRetry(ctx context.Context) error {
	if jitter := s.randomUpTo(s.cfg.Jitter); jitter > 0 {
		s.log.Info("jittering before backup", "jitter", jitter.Round(time.Second))
		if !s.sleep(ctx, jitter) {
			return ctx.Err()
		}
	}

	attempts := 0
	for {
		attempts++
		_, err := s.attempt(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		if errors.Is(err, ErrTerminal) {
			return err
		}
		if s.cfg.MaxRetries >= 0 && attempts > s.cfg.MaxRetries {
			s.log.Error("giving up on this backup", "attempts", attempts, "err", err)
			return err
		}

		backoff := s.backoff(attempts)
		s.log.Warn("backup failed, will retry",
			"attempts", attempts, "retry_in", backoff.Round(time.Second), "err", err)
		if !s.sleep(ctx, backoff) {
			return err
		}
	}
}

// backoff is linear in the attempt count, capped at five times the base
// interval, with equal jitter so a fleet does not converge on the same instant.
func (s *Scheduler) backoff(attempts int) time.Duration {
	base := s.cfg.RetryInterval * time.Duration(attempts)
	if max := s.cfg.RetryInterval * 5; base > max {
		base = max
	}
	half := base / 2
	if half <= 0 {
		return base
	}
	return half + time.Duration(rand.Int64N(int64(half)))
}

// attempt marks the attempt running, runs it, and records the outcome.
func (s *Scheduler) attempt(ctx context.Context) (status.Outcome, error) {
	start := s.now()
	_ = s.store.Update(func(st *status.Status) { st.MarkRunning(start) })

	outcome, err := s.run(ctx)
	end := s.now()

	if outcome.Elapsed == 0 {
		// The run func may not measure elapsed time itself; the scheduler's own
		// wall clock is authoritative because it includes everything from
		// marking the attempt to recording it, not just restic's execution.
		outcome.Elapsed = end.Sub(start)
	}
	if err != nil && outcome.Err == nil {
		outcome.Err = err
	}

	_ = s.store.Update(func(st *status.Status) { st.MarkFinished(end, outcome) })
	return outcome, err
}

// randomUpTo returns a random duration in [0, d).
func (s *Scheduler) randomUpTo(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d)))
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// IsTerminal reports whether err was marked Terminal. The scheduler uses it to
// decide whether to keep trying; callers use it to check how a failure was
// classified.
func IsTerminal(err error) bool { return errors.Is(err, ErrTerminal) }

// Terminal wraps err so the scheduler will not retry it.
func Terminal(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrTerminal, err)
}
