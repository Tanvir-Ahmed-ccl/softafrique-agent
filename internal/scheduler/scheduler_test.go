package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"softafrique-backup-agent/internal/config"
	"softafrique-backup-agent/internal/status"
)

type logDiscard struct{}

func (logDiscard) Write(p []byte) (int, error) { return len(p), nil }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(logDiscard{}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// testClock is a clock the test controls, so the timing rules can be checked
// without waiting minutes for them.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }

func (c *testClock) Sleep(ctx context.Context, d time.Duration) bool {
	c.now = c.now.Add(d)
	return true
}

// harness pairs a scheduler with its clock so a test can make a run "take time"
// by advancing the clock from inside the run function.
type harness struct {
	*Scheduler
	clock *testClock
}

// testScheduler returns a scheduler whose clock and sleeps are under the test's
// control.
func testScheduler(t *testing.T, run RunFunc, cfgCb func(*config.Config)) *harness {
	t.Helper()
	cfg := config.Default()
	cfg.ScheduleInterval = time.Hour
	cfg.RetryInterval = 5 * time.Minute
	cfg.Jitter = 0
	if cfgCb != nil {
		cfgCb(cfg)
	}
	store := status.NewStore(filepath.Join(t.TempDir(), "status.json"))
	s := New(cfg, store, run, testLogger())
	clock := &testClock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
	s.now = clock.Now
	s.sleep = clock.Sleep
	return &harness{Scheduler: s, clock: clock}
}

func TestWaitUntilDueFreshInstallRunsImmediately(t *testing.T) {
	s := testScheduler(t, nil, nil)
	if d := s.waitUntilDue(status.New()); d != 0 {
		t.Errorf("a device that has never succeeded should run now, got wait %v", d)
	}
}

func TestWaitUntilDueFailedRunStillCountsSchedule(t *testing.T) {
	// A failed attempt does not reset the schedule: the last success is what
	// decides when the next run is due, so a device that failed an hour after a
	// good backup does not back up again immediately and then again on schedule.
	s := testScheduler(t, nil, nil)
	st := status.New()
	st.LastSuccess = status.Stamp(s.now().Add(-30 * time.Minute))
	st.LastAttemptStatus = status.AttemptFailed
	d := s.waitUntilDue(st)
	if d != 30*time.Minute {
		t.Errorf("want 30m until the next scheduled run, got %v", d)
	}
}

func TestWaitUntilDueSuspendedWaitsRetryInterval(t *testing.T) {
	s := testScheduler(t, nil, nil)
	st := status.New()
	st.LastSuccess = status.Stamp(s.now().Add(-time.Hour))
	st.LastAttemptStatus = status.AttemptSuspended
	if d := s.waitUntilDue(st); d != 5*time.Minute {
		t.Errorf("a suspended device should idle for a retry interval, got %v", d)
	}
}

func TestWaitUntilDueOverdueRunsNow(t *testing.T) {
	s := testScheduler(t, nil, nil)
	st := status.New()
	st.LastSuccess = status.Stamp(s.now().Add(-2 * time.Hour))
	if d := s.waitUntilDue(st); d != 0 {
		t.Errorf("an overdue device should run now, got wait %v", d)
	}
}

func TestWaitUntilDueUnparseableSuccessRunsNow(t *testing.T) {
	s := testScheduler(t, nil, nil)
	st := status.New()
	st.LastSuccess = "not a timestamp"
	if d := s.waitUntilDue(st); d != 0 {
		t.Errorf("an unreadable timestamp should not wedge the schedule, got %v", d)
	}
}

func TestAttemptRecordsElapsedNotResticTime(t *testing.T) {
	h := testScheduler(t, nil, nil)
	s := h.Scheduler
	s.run = func(ctx context.Context) (status.Outcome, error) {
		// The run spends three and a half minutes waiting for the network and
		// then one second of actual work. Finding 7 is that the agent must report
		// the 211 seconds, not restic's 1.
		h.clock.now = h.clock.now.Add(3*time.Minute + 33*time.Second)
		return status.Outcome{
			SnapshotID:     "snap1",
			ResticDuration: 1 * time.Second,
		}, nil
	}
	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := s.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.LastAttemptStatus != status.AttemptSuccess {
		t.Errorf("status = %q", st.LastAttemptStatus)
	}
	if st.LastDurationSeconds != 213 {
		t.Errorf("elapsed = %vs, want 213: this is the number that must not be 1",
			st.LastDurationSeconds)
	}
	if st.ResticDurationSeconds != 1 {
		t.Errorf("restic duration = %v, want 1 (kept separately)", st.ResticDurationSeconds)
	}
	if st.LastAttemptStart == "" || st.LastAttemptEnd == "" {
		t.Error("both attempt timestamps must be set: finding 3 asked for last attempt as distinct from last success")
	}
	if st.LastSuccess != st.LastAttemptEnd {
		t.Error("a successful attempt should also set last_success")
	}
}

func TestFailureRecordsErrorAndDoesNotMoveLastSuccess(t *testing.T) {
	s := testScheduler(t, nil, nil)
	s.run = func(ctx context.Context) (status.Outcome, error) {
		return status.Outcome{}, errors.New("network unreachable")
	}
	if _, err := s.RunOnce(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	st, err := s.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.LastAttemptStatus != status.AttemptFailed {
		t.Errorf("status = %q, want failed", st.LastAttemptStatus)
	}
	if st.LastSuccess != "" {
		t.Errorf("last_success = %q, want it still empty", st.LastSuccess)
	}
	if st.ConsecutiveFailures != 1 {
		t.Errorf("consecutive_failures = %d", st.ConsecutiveFailures)
	}
}

func TestRetryHappensThenGivesUp(t *testing.T) {
	s := testScheduler(t, nil, func(c *config.Config) { c.MaxRetries = 2 })
	var attempts int
	s.run = func(ctx context.Context) (status.Outcome, error) {
		attempts++
		return status.Outcome{}, errors.New("boom")
	}
	if err := s.runWithRetry(context.Background()); err == nil {
		t.Fatal("expected the final error")
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 (initial + 2 retries)", attempts)
	}
}

func TestRetryIsUnlimitedByDefault(t *testing.T) {
	s := testScheduler(t, nil, func(c *config.Config) { c.MaxRetries = -1 })
	var attempts int
	s.run = func(ctx context.Context) (status.Outcome, error) {
		attempts++
		if attempts == 4 {
			return status.Outcome{SnapshotID: "ok"}, nil
		}
		return status.Outcome{}, errors.New("still offline")
	}
	if err := s.runWithRetry(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts != 4 {
		t.Errorf("attempts = %d, want the loop to keep trying until success", attempts)
	}
}

func TestTerminalErrorIsNotRetried(t *testing.T) {
	s := testScheduler(t, nil, nil)
	var attempts int
	s.run = func(ctx context.Context) (status.Outcome, error) {
		attempts++
		return status.Outcome{}, Terminal(errors.New("gateway rejected the device credentials"))
	}
	err := s.runWithRetry(context.Background())
	if !errors.Is(err, ErrTerminal) {
		t.Fatalf("want ErrTerminal, got %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want exactly 1: a revoked device must not be retried", attempts)
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	s := testScheduler(t, nil, func(c *config.Config) { c.RetryInterval = 4 * time.Minute })
	// Equal jitter: the result is always in [d/2, d), so a fleet never
	// synchronises, and no attempt retries instantly.
	for attempts := 1; attempts <= 8; attempts++ {
		got := s.backoff(attempts)
		want := 4 * time.Minute * time.Duration(min(attempts, 5))
		if got < want/2 || got >= want {
			t.Errorf("backoff(%d) = %v, want it within [%v, %v)", attempts, got, want/2, want)
		}
	}
}

func TestRandomUpTo(t *testing.T) {
	s := testScheduler(t, nil, nil)
	if got := s.randomUpTo(0); got != 0 {
		t.Errorf("randomUpTo(0) = %v, want 0", got)
	}
	if got := s.randomUpTo(-time.Second); got != 0 {
		t.Errorf("a negative jitter must be treated as none, got %v", got)
	}
	for i := 0; i < 50; i++ {
		if got := s.randomUpTo(time.Second); got < 0 || got >= time.Second {
			t.Fatalf("randomUpTo(1s) = %v, out of range", got)
		}
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	s := testScheduler(t, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	s.sleep = func(context.Context, time.Duration) bool { return false }
	s.run = func(ctx context.Context) (status.Outcome, error) { return status.Outcome{}, nil }
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run should exit cleanly on cancellation, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on context cancellation")
	}
}

func TestJitterIsAppliedBeforeFirstAttempt(t *testing.T) {
	s := testScheduler(t, nil, func(c *config.Config) { c.Jitter = 20 * time.Second })
	var slept []time.Duration
	s.sleep = func(ctx context.Context, d time.Duration) bool {
		slept = append(slept, d)
		return true
	}
	s.run = func(ctx context.Context) (status.Outcome, error) { return status.Outcome{}, nil }
	if err := s.runWithRetry(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(slept) == 0 {
		t.Fatal("expected a startup jitter sleep")
	}
	if slept[0] < 0 || slept[0] >= 20*time.Second {
		t.Errorf("jitter %v outside [0, 20s)", slept[0])
	}
}
