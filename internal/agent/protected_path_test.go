package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"softafrique-backup-agent/internal/api"
	"softafrique-backup-agent/internal/scheduler"
	"softafrique-backup-agent/internal/status"
)

// These cover the behaviour asked for after a customer's protected folder was
// deleted after installation. The agent recreates the folder so a restore has a
// destination, but it does not snapshot the empty directory and call that
// protection. It records an action-required failure instead, which the CLI
// returns as exit code 4.

// The headline case: a folder deleted after installation is recreated, but
// restic is not run and the operator is told to restore the data.
func TestDeletedProtectedFolderIsRecreatedButNotBackedUp(t *testing.T) {
	protected := customerDataDir(t)
	gw := &fakeGateway{config: &api.RemoteConfig{Status: api.StateActive, BackupPath: protected}}
	rest := &fakeRestic{}
	a := newTestAgent(t, gw, rest)
	enrollTest(t, a)

	if _, err := a.BackupNow(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Someone deletes the customer's data folder.
	if err := os.RemoveAll(protected); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(protected); !os.IsNotExist(err) {
		t.Fatal("the test did not remove the folder")
	}

	outcome, err := a.BackupNow(context.Background())
	if !errors.Is(err, ErrActionRequired) {
		t.Fatalf("err = %v, want ErrActionRequired", err)
	}
	if !scheduler.IsTerminal(err) {
		t.Error("a missing protected folder needs a human, not an automatic retry")
	}
	if len(outcome.RecreatedPaths) != 1 || outcome.RecreatedPaths[0] != protected {
		t.Errorf("RecreatedPaths = %v, want [%s]", outcome.RecreatedPaths, protected)
	}
	if info, err := os.Stat(protected); err != nil || !info.IsDir() {
		t.Fatalf("the folder was not recreated: %v", err)
	}
	if rest.backups != 1 {
		t.Errorf("restic ran %d times, want 1: the empty recreated folder must not be snapshotted", rest.backups)
	}

	st, err := a.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d, want 0: the customer's data is missing, not the repository", st.ConsecutiveFailures)
	}
	if st.LastAttemptStatus != status.AttemptRecreated {
		t.Errorf("last attempt = %q, want %q", st.LastAttemptStatus, status.AttemptRecreated)
	}
	if st.ActionRequired == "" {
		t.Error("action_required must be set, or nothing alerts on this")
	}
}

// The reason this is not reported as a plain success: the recreated directory
// is empty and no snapshot is taken, so nothing was actually protected.
// last_success must not move, or the staleness threshold can never fire.
func TestRecreatedFolderDoesNotCountAsProtected(t *testing.T) {
	protected := customerDataDir(t)
	gw := &fakeGateway{config: &api.RemoteConfig{Status: api.StateActive, BackupPath: protected}}
	a := newTestAgent(t, gw, &fakeRestic{})
	enrollTest(t, a)

	if _, err := a.BackupNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := a.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if first.LastSuccess == "" {
		t.Fatal("the first run should have set last_success")
	}

	if err := os.RemoveAll(protected); err != nil {
		t.Fatal(err)
	}
	if _, err := a.BackupNow(context.Background()); !errors.Is(err, ErrActionRequired) {
		t.Fatalf("err = %v, want ErrActionRequired", err)
	}
	st, err := a.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.LastSuccess != first.LastSuccess {
		t.Errorf("last_success moved from %q to %q; an empty folder is not protection",
			first.LastSuccess, st.LastSuccess)
	}
	if st.LastSnapshotID != first.LastSnapshotID {
		t.Errorf("last_snapshot_id moved to %q; the empty snapshot must not be recorded as the last good one",
			st.LastSnapshotID)
	}
	// A 0.1.0-era monitor reads this field and nothing else.
	if st.LastBackupStatus != status.AttemptFailed {
		t.Errorf("last_backup_status = %q, want %q", st.LastBackupStatus, status.AttemptFailed)
	}
	if st.ActionRequired == "" {
		t.Error("action_required must be set, or nothing alerts on this")
	}
	if !strings.Contains(st.ActionRequired, protected) {
		t.Errorf("action_required %q does not name the path, so it cannot be acted on without a site visit",
			st.ActionRequired)
	}
}

// The gateway must not be told a device is protected when it is not.
func TestRecreatedFolderIsNotReportedToTheGatewayAsSuccess(t *testing.T) {
	protected := customerDataDir(t)
	gw := &fakeGateway{config: &api.RemoteConfig{Status: api.StateActive, BackupPath: protected}}
	a := newTestAgent(t, gw, &fakeRestic{})
	enrollTest(t, a)

	if _, err := a.BackupNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(protected); err != nil {
		t.Fatal(err)
	}
	if _, err := a.BackupNow(context.Background()); !errors.Is(err, ErrActionRequired) {
		t.Fatalf("err = %v, want ErrActionRequired", err)
	}

	if len(gw.reports) == 0 {
		t.Fatal("nothing was reported")
	}
	last := gw.reports[len(gw.reports)-1]
	if last.LastBackupStatus == "success" {
		t.Error("a device whose folder was recreated must not report success to the gateway")
	}
	if last.LastBackupError == "" {
		t.Error("the report should say why")
	}
}

// Once the folder is genuinely back with data in it, the notice has to clear, or
// it alerts forever.
func TestRecreateNoticeClearsOnACleanRun(t *testing.T) {
	protected := customerDataDir(t)
	gw := &fakeGateway{config: &api.RemoteConfig{Status: api.StateActive, BackupPath: protected}}
	a := newTestAgent(t, gw, &fakeRestic{})
	enrollTest(t, a)

	if err := os.RemoveAll(protected); err != nil {
		t.Fatal(err)
	}
	if _, err := a.BackupNow(context.Background()); !errors.Is(err, ErrActionRequired) {
		t.Fatalf("err = %v, want ErrActionRequired", err)
	}
	st, err := a.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.ActionRequired == "" {
		t.Fatal("the first run over a missing folder should set action_required")
	}

	// The customer puts their data back.
	if err := os.MkdirAll(protected, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(protected, "data.db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.BackupNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err = a.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.ActionRequired != "" {
		t.Errorf("action_required = %q, want it cleared once a real backup ran", st.ActionRequired)
	}
	if st.LastAttemptStatus != status.AttemptSuccess {
		t.Errorf("last attempt = %q, want %q", st.LastAttemptStatus, status.AttemptSuccess)
	}
	if st.LastSuccess == "" {
		t.Error("last_success should be set by the clean run")
	}
}

// A pending re-enroll instruction belongs to the gateway, not to us. Clearing
// our own notice must not delete it.
func TestRecreateNoticeDoesNotClearAGatewayReenrollInstruction(t *testing.T) {
	s := status.New()
	s.ActionRequired = "re-enroll: the gateway has withdrawn this device"
	s.LastAttemptStatus = status.AttemptRecreated
	s.MarkFinished(time.Now(), status.Outcome{SnapshotID: "snap-9"})
	if s.ActionRequired == "" {
		t.Error("a clean run deleted the gateway's re-enroll instruction")
	}
}

// The recreate must not hide a real restic failure. Give the folder real
// content so the backup actually runs, then make restic fail.
func TestRecreateDoesNotHideABackupFailure(t *testing.T) {
	protected := customerDataDir(t)
	if err := os.WriteFile(filepath.Join(protected, "data.db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gw := &fakeGateway{config: &api.RemoteConfig{Status: api.StateActive, BackupPath: protected}}
	rest := &fakeRestic{err: errors.New("restic: snapshot failed")}
	a := newTestAgent(t, gw, rest)
	enrollTest(t, a)

	if _, err := a.BackupNow(context.Background()); err == nil {
		t.Fatal("expected the backup error to surface")
	}
	st, err := a.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.LastAttemptStatus != status.AttemptFailed {
		t.Errorf("last attempt = %q, want %q", st.LastAttemptStatus, status.AttemptFailed)
	}
	if st.ConsecutiveFailures != 1 {
		t.Errorf("consecutive_failures = %d, want 1", st.ConsecutiveFailures)
	}
}

// ensureBackupPaths must never delete anything to make room for a directory.
func TestEnsureBackupPathsLeavesAFileAlone(t *testing.T) {
	a := newTestAgent(t, &fakeGateway{}, &fakeRestic{})
	file := filepath.Join(t.TempDir(), "customer-data")
	if err := os.WriteFile(file, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}

	created := a.ensureBackupPaths([]string{file})
	if len(created) != 0 {
		t.Errorf("created = %v, want nothing: the path is a file", created)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("the file was removed: %v", err)
	}
	if string(data) != "precious" {
		t.Error("the file's contents changed")
	}
}

// Several configured folders, only one of them deleted: only that one is
// reported, and the notice names it.
func TestEnsureBackupPathsReportsOnlyWhatItCreated(t *testing.T) {
	a := newTestAgent(t, &fakeGateway{}, &fakeRestic{})
	root := t.TempDir()
	present := filepath.Join(root, "Present")
	gone := filepath.Join(root, "Gone")
	if err := os.MkdirAll(present, 0o755); err != nil {
		t.Fatal(err)
	}

	created := a.ensureBackupPaths([]string{present, gone, ""})
	if len(created) != 1 || created[0] != gone {
		t.Errorf("created = %v, want just [%s]", created, gone)
	}
}

// A fixed disk is not a licence to back up the operating system or the agent's
// own key store. The installer checks the typed path; this catches a path the
// gateway supplies later.
func TestSystemAndAgentDirectoriesAreRefusedBeforeRestic(t *testing.T) {
	for _, path := range []string{`C:\Windows`, `C:\Program Files`, `C:\ProgramData`} {
		gw := &fakeGateway{config: &api.RemoteConfig{Status: api.StateActive, BackupPath: path}}
		rest := &fakeRestic{}
		a := newTestAgent(t, gw, rest)
		enrollTest(t, a)

		_, err := a.RunBackup(context.Background())
		if err == nil {
			t.Errorf("%s was backed up", path)
			continue
		}
		if !scheduler.IsTerminal(err) {
			t.Errorf("%s: err = %v, want terminal", path, err)
		}
		if rest.backups != 0 {
			t.Errorf("%s: restic ran %d times", path, rest.backups)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error %q does not name %s", err, path)
		}
	}

	// The data directory is supplied by config, not a fixed Windows path.
	a := newTestAgent(t, &fakeGateway{}, &fakeRestic{})
	if got := a.protectedPaths([]string{a.cfg.DataDir}); len(got) != 1 {
		t.Errorf("the agent data directory was accepted: %v", got)
	}
}

// The recreation guard has to survive the next run: after the first run creates
// the folder, the missing-path check no longer fires, so without a persistent
// marker the retry would snapshot the empty folder. Once real data is back, the
// notice must clear itself.
func TestRecreatedFolderStaysActionRequiredUntilDataReturns(t *testing.T) {
	protected := customerDataDir(t)
	gw := &fakeGateway{config: &api.RemoteConfig{Status: api.StateActive, BackupPath: protected}}
	rest := &fakeRestic{}
	a := newTestAgent(t, gw, rest)
	enrollTest(t, a)

	if _, err := a.BackupNow(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(protected); err != nil {
		t.Fatal(err)
	}
	if _, err := a.BackupNow(context.Background()); !errors.Is(err, ErrActionRequired) {
		t.Fatalf("first run over the missing folder: err = %v, want ErrActionRequired", err)
	}
	// The folder now exists but is empty. This is the run that used to go green.
	if _, err := a.BackupNow(context.Background()); !errors.Is(err, ErrActionRequired) {
		t.Fatalf("second run over the recreated folder: err = %v, want ErrActionRequired", err)
	}
	if rest.backups != 1 {
		t.Fatalf("restic ran %d times, want 1: an empty recreated folder must never be snapshotted", rest.backups)
	}

	// The customer's data is restored; the next run protects it and clears the
	// local instruction.
	if err := os.WriteFile(filepath.Join(protected, "data.db"), []byte("customer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.BackupNow(context.Background()); err != nil {
		t.Fatalf("backup after restore: %v", err)
	}
	st, err := a.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.ActionRequired != "" {
		t.Errorf("action_required = %q, want empty after a real backup", st.ActionRequired)
	}
	if st.LastAttemptStatus != status.AttemptSuccess {
		t.Errorf("last attempt = %q, want success", st.LastAttemptStatus)
	}
}

// ---- network shares ---------------------------------------------------------
//
// Mohsin's policy: a network share is refused by default and allowed only
// behind an explicit opt-in, because the service runs as LocalSystem and so
// reaches a share as the machine account rather than as the technician who set
// it up. The installer already applies that to what a technician typed; these
// cover the half that catches what the *gateway* said, in a backup_path on
// /config, which no amount of installer checking would see.

// The refusal has to stop the attempt, not just the folder creation. Handing a
// refused path to restic anyway is the same defect with one more step in it.
func TestANetworkShareIsRefusedBeforeResticIsGivenThePath(t *testing.T) {
	const share = `\\fileserver\CustomerData`
	gw := &fakeGateway{config: &api.RemoteConfig{Status: api.StateActive, BackupPath: share}}
	rest := &fakeRestic{}
	a := newTestAgent(t, gw, rest)
	enrollTest(t, a)

	_, err := a.RunBackup(context.Background())
	if err == nil {
		t.Fatal("a network share was backed up with no opt-in")
	}
	if !scheduler.IsTerminal(err) {
		t.Errorf("err = %v, want it terminal: retrying every five minutes against an unreadable share buries it", err)
	}
	if rest.backups != 0 {
		t.Errorf("restic ran %d times; it must never be handed the refused path", rest.backups)
	}
	for _, want := range []string{share, "LocalSystem", "allow_unc"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// The sentence lands in status.json, because that is what a support engineer
// reads at three in the morning and a bare "suspended" says nothing about which
// of the reasons there is a share involved.
func TestANetworkShareRefusalIsRecordedWithItsReason(t *testing.T) {
	const share = `\\fileserver\CustomerData`
	gw := &fakeGateway{config: &api.RemoteConfig{Status: api.StateActive, BackupPath: share}}
	a := newTestAgent(t, gw, &fakeRestic{})
	enrollTest(t, a)

	if _, err := a.BackupNow(context.Background()); err == nil {
		t.Fatal("expected an error")
	}

	st, err := a.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.LastAttemptStatus == status.AttemptSuccess {
		t.Error("last_attempt_status reports success; nothing was protected, so this would be the 0.1.0 lie")
	}
	if !strings.Contains(st.LastAttemptError, share) {
		t.Errorf("last_attempt_error = %q, want it to name the refused path", st.LastAttemptError)
	}
	// Nothing failed locally, so the failure counter must not climb: this is the
	// same rule the suspended case follows, and it keeps the counter meaningful on
	// the machines that are actually broken.
	if st.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d, want 0: the agent refused on purpose, and this is not a failure to retry",
			st.ConsecutiveFailures)
	}
}

// The opt-in exists, so it has to work, or it is not an opt-in.
func TestTheOptInAllowsANetworkShare(t *testing.T) {
	const share = `\\fileserver\CustomerData`
	gw := &fakeGateway{config: &api.RemoteConfig{Status: api.StateActive, BackupPath: share}}
	rest := &fakeRestic{}
	a := newTestAgent(t, gw, rest)
	enrollTest(t, a)
	a.cfg.AllowUNC = true

	if _, err := a.RunBackup(context.Background()); err != nil {
		t.Fatalf("a share with the opt-in was refused: %v", err)
	}
	if rest.backups != 1 {
		t.Errorf("restic ran %d times, want 1", rest.backups)
	}
}

// Local paths are never affected by the opt-in, in either direction. A false
// positive here is worse than a missing refusal: it would refuse the customer's
// own D: drive and report it as a network-share policy decision.
func TestLocalPathsAreNeverTreatedAsShares(t *testing.T) {
	a := newTestAgent(t, &fakeGateway{}, &fakeRestic{})
	local := []string{`C:\SoftafriqueBackup`, `D:\Customer\Data`, `C:\`}
	if got := a.refusedPaths(local); len(got) != 0 {
		t.Errorf("refused %v; only a share is ever refused here", got)
	}
	a.cfg.AllowUNC = true
	share := `\\server\share`
	if got := a.refusedPaths([]string{local[0], share}); len(got) != 0 {
		t.Errorf("refused %v with the opt-in set", got)
	}
}
