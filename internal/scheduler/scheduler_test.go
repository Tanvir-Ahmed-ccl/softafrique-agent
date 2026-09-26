package scheduler

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"softafrique-backup-agent/internal/config"
	"softafrique-backup-agent/internal/status"
)

func testScheduler(t *testing.T, cfgCb func(*config.Config)) *Scheduler {
	t.Helper()
	cfg := config.Default()
	cfg.ScheduleInterval = 1 * time.Hour
	if cfgCb != nil {
		cfgCb(cfg)
	}
	store := status.NewStore(filepath.Join(t.TempDir(), "status.json"))
	l := slog.New(slog.NewTextHandler(logDiscard{}, nil))
	return New(cfg, store, func(ctx context.Context) error { return nil }, l)
}

type logDiscard struct{}

func (logDiscard) Write(p []byte) (int, error) { return len(p), nil }

func TestWaitUntilDue_FreshInstallRunsImmediately(t *testing.T) {
	s := testScheduler(t, nil)
	if d := s.waitUntilDue(context.Background(), &status.Status{}); d != 0 {
		t.Errorf("fresh status should run immediately, got wait %v", d)
	}
	if d := s.waitUntilDue(context.Background(), nil); d != 0 {
		t.Errorf("nil status should run immediately, got wait %v", d)
	}
}

func TestWaitUntilDue_FailedRunRunsImmediately(t *testing.T) {
	s := testScheduler(t, nil)
	st := &status.Status{
		LastBackupTime:   time.Now().Add(-time.Hour).Format(time.RFC3339),
		LastBackupStatus: "failed",
	}
	if d := s.waitUntilDue(context.Background(), st); d != 0 {
		t.Errorf("failed status should run immediately, got wait %v", d)
	}
}

func TestWaitUntilDue_NotDueYet(t *testing.T) {
	s := testScheduler(t, nil)
	st := &status.Status{
		LastBackupTime:   time.Now().Format(time.RFC3339),
		LastBackupStatus: "success",
	}
	d := s.waitUntilDue(context.Background(), st)
	if d <= 0 || d > time.Hour {
		t.Errorf("expected wait between 0 and 1h, got %v", d)
	}
}

func TestWaitUntilDue_Overdue(t *testing.T) {
	s := testScheduler(t, nil)
	st := &status.Status{
		LastBackupTime:   time.Now().Add(-2 * time.Hour).Format(time.RFC3339),
		LastBackupStatus: "success",
	}
	if d := s.waitUntilDue(context.Background(), st); d != 0 {
		t.Errorf("overdue status should run immediately, got wait %v", d)
	}
}