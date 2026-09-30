package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"softafrique-backup-agent/internal/api"
	"softafrique-backup-agent/internal/config"
	"softafrique-backup-agent/internal/restic"
	"softafrique-backup-agent/internal/scheduler"
	"softafrique-backup-agent/internal/secret"
	"softafrique-backup-agent/internal/status"
)

// fakeGateway is a scripted gateway.
type fakeGateway struct {
	config    *api.RemoteConfig
	configErr error
	reports   []api.StatusReport
	reportErr error
	healthErr error
}

func (f *fakeGateway) Config(context.Context) (*api.RemoteConfig, error) {
	if f.configErr != nil {
		return nil, f.configErr
	}
	if f.config == nil {
		return &api.RemoteConfig{Status: api.StateActive}, nil
	}
	return f.config, nil
}

func (f *fakeGateway) PostStatus(_ context.Context, r api.StatusReport) error {
	f.reports = append(f.reports, r)
	return f.reportErr
}

func (f *fakeGateway) Health(context.Context) error { return f.healthErr }

// fakeRestic records what it was asked to do.
type fakeRestic struct {
	backups    int
	include    []string
	result     *restic.Result
	err        error
	pingErr    error
	statsCalls int
}

func (f *fakeRestic) Ping(context.Context) error { return f.pingErr }

func (f *fakeRestic) Backup(_ context.Context, include, _ []string) (*restic.Result, error) {
	f.backups++
	f.include = include
	if f.err != nil {
		return nil, f.err
	}
	if f.result == nil {
		return &restic.Result{SnapshotID: "snap-1"}, nil
	}
	return f.result, nil
}

func (f *fakeRestic) Stats(context.Context, string) (*restic.Stats, error) {
	f.statsCalls++
	return &restic.Stats{TotalSize: 1024, SnapshotsCount: 3}, nil
}

func newTestAgent(t *testing.T, gw gateway, rest backupRunner) *Agent {
	t.Helper()
	return newTestAgentIn(t, t.TempDir(), gw, rest)
}

// newTestAgentIn builds an agent over a caller-chosen data directory, so a test
// can simulate a restart on the same machine.
func newTestAgentIn(t *testing.T, dir string, gw gateway, rest backupRunner) *Agent {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.StatusFile = filepath.Join(dir, "status.json")
	cfg.LogFile = filepath.Join(dir, "agent.log")
	cfg.Include = nil
	cfg.AllowUNC = false
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	a := New(cfg, status.NewStore(cfg.StatusFile), discardLogger()).withDeps(gw, rest)
	a.Version = "0.2.0-test"
	return a
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// enrollTest writes a credentials blob so load() succeeds.
func enrollTest(t *testing.T, a *Agent) {
	t.Helper()
	creds := &secret.Credentials{
		DeviceID:       "dev-01HQ8",
		DevicePassword: "device-pass",
		EncryptionKey:  "encryption-key",
		Repository:     "rest:https://backup.softafrique.net/dev-01HQ8",
		Tenant:         "acme",
		BackupPath:     customerDataDir(t),
		Server:         api.DefaultBaseURL,
		EnrolledAt:     status.Now(),
	}
	if err := creds.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := a.cfg.SecretStore().Save(creds); err != nil {
		t.Fatal(err)
	}
}

// customerDataDir is a real, existing directory to stand in for the customer's
// data folder.
//
// These tests used to use the literal string "C:\CustomerData", which is a
// perfectly legal *filename* on macOS and Linux. Nothing created paths then, so
// it was harmless; now that the agent recreates a protected folder that has gone
// missing, a Windows-style fixture path became a real directory in the package
// directory, committed or not. Fixtures that the production code creates things
// from have to be real paths.
func customerDataDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "CustomerData")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// An unenrolled device must not attempt a backup, and must not retry forever.
func TestUnenrolledDeviceIsTerminal(t *testing.T) {
	rest := &fakeRestic{}
	a := newTestAgent(t, &fakeGateway{}, rest)

	outcome, err := a.RunBackup(context.Background())
	if !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("err = %v, want ErrNotEnrolled", err)
	}
	if !scheduler.IsTerminal(err) {
		t.Error("an unenrolled device is a terminal condition: retrying cannot fix it")
	}
	if rest.backups != 0 {
		t.Errorf("restic ran %d times on an unenrolled device", rest.backups)
	}
	_ = outcome
}

// A suspended device must not write to the repository, and must not be retried:
// this is the whole point of an operator being able to stop a device.
func TestSuspendedDeviceDoesNotBackUpAndIsNotRetried(t *testing.T) {
	gw := &fakeGateway{config: &api.RemoteConfig{
		DeviceID:   "dev-01HQ8",
		Status:     api.StateSuspended,
		BackupPath: customerDataDir(t),
	}}
	rest := &fakeRestic{}
	a := newTestAgent(t, gw, rest)
	enrollTest(t, a)

	_, err := a.RunBackup(context.Background())
	if err == nil {
		t.Fatal("a suspended device must return an error")
	}
	if !scheduler.IsTerminal(err) {
		t.Error("a suspended device is terminal: the gateway will not unsuspend it by retrying")
	}
	if rest.backups != 0 {
		t.Errorf("restic ran %d times while the device was suspended", rest.backups)
	}
	if suspended, why := a.Suspension(); !suspended || why == "" {
		t.Errorf("Suspension() = %v, %q; want a reason for the operator", suspended, why)
	}
	st, err := a.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.LastAttemptStatus != status.AttemptSuspended {
		t.Errorf("last attempt = %q, want %q: suspended is not a failure",
			st.LastAttemptStatus, status.AttemptSuspended)
	}
}

// A status the agent does not recognise must stop backups too. An unknown state
// on a decommissioned device must never be read as permission to keep writing.
func TestUnknownGatewayStatusSuspends(t *testing.T) {
	gw := &fakeGateway{config: &api.RemoteConfig{Status: api.DeviceState("decommissioned")}}
	rest := &fakeRestic{}
	a := newTestAgent(t, gw, rest)
	enrollTest(t, a)

	if _, err := a.RunBackup(context.Background()); err == nil {
		t.Fatal("an unrecognised device state must not permit backups")
	}
	if rest.backups != 0 {
		t.Errorf("restic ran %d times with an unrecognised device state", rest.backups)
	}
}

// The gateway being unreachable must not stop backups. This is the requirement
// behind "network failure keeps the cached config and backups running".
func TestGatewayOutageStillBacksUp(t *testing.T) {
	gw := &fakeGateway{configErr: &api.Error{Op: "config", Err: api.ErrUnavailable}}
	rest := &fakeRestic{}
	a := newTestAgent(t, gw, rest)
	enrollTest(t, a)

	if _, err := a.RunBackup(context.Background()); err != nil {
		t.Fatalf("a gateway outage must not fail the backup: %v", err)
	}
	if rest.backups != 1 {
		t.Errorf("restic ran %d times, want 1", rest.backups)
	}
}

// A gateway outage is not terminal, because the next poll may succeed.
func TestGatewayOutageIsNotTerminal(t *testing.T) {
	gw := &fakeGateway{configErr: &api.Error{Op: "config", Err: api.ErrUnavailable}}
	a := newTestAgent(t, gw, &fakeRestic{})
	enrollTest(t, a)
	if err := a.RefreshConfig(context.Background()); scheduler.IsTerminal(err) {
		t.Error("a transport failure must stay retryable")
	}
}

// A 401 from /config means the device is no longer enrolled. That is terminal
// and it must be visible in the status file.
func TestRejectedCredentialsSuspendAndAreActionable(t *testing.T) {
	gw := &fakeGateway{configErr: &api.Error{Op: "config", StatusCode: 401, Err: api.ErrUnauthorized}}
	rest := &fakeRestic{}
	a := newTestAgent(t, gw, rest)
	enrollTest(t, a)

	_, err := a.RunBackup(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if rest.backups != 0 {
		t.Errorf("restic ran %d times after the gateway rejected the device", rest.backups)
	}
	st, err := a.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.ActionRequired == "" {
		t.Error("status.json must tell a human what to do when the gateway rejects the device")
	}
	if !strings.Contains(strings.ToLower(st.ActionRequired), "re-enroll") {
		t.Errorf("action_required = %q, want it to mention re-enrolling", st.ActionRequired)
	}
}

// A successful backup reports to the gateway with the contract's fields and
// nothing else.
func TestSuccessfulBackupIsReported(t *testing.T) {
	gw := &fakeGateway{config: &api.RemoteConfig{
		DeviceID:   "dev-01HQ8",
		Status:     api.StateActive,
		BackupPath: customerDataDir(t),
	}}
	rest := &fakeRestic{result: &restic.Result{
		SnapshotID:   "abcdef123456",
		FilesAdded:   10,
		FilesChanged: 2,
		BytesAdded:   4096,
	}}
	a := newTestAgent(t, gw, rest)
	enrollTest(t, a)

	outcome, err := a.RunBackup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.SnapshotID != "abcdef123456" {
		t.Errorf("snapshot = %q", outcome.SnapshotID)
	}
	if rest.backups != 1 {
		t.Fatalf("restic ran %d times", rest.backups)
	}
	// The gateway-assigned folder is what gets backed up.
	if len(rest.include) != 1 || !strings.Contains(rest.include[0], "CustomerData") {
		t.Errorf("backed up %v, want the gateway's backup_path", rest.include)
	}
	if len(gw.reports) != 1 {
		t.Fatalf("sent %d status reports, want 1", len(gw.reports))
	}
	rep := gw.reports[0]
	if rep.LastBackupStatus != "success" {
		t.Errorf("last_backup_status = %q, want success", rep.LastBackupStatus)
	}
	if rep.LastSnapshotID != "abcdef123456" {
		t.Errorf("last_snapshot_id = %q", rep.LastSnapshotID)
	}
	if rep.LastBackupError != "" {
		t.Errorf("a success must not carry an error: %q", rep.LastBackupError)
	}
	if rep.AgentVersion != a.Version {
		t.Errorf("agent_version = %q, want %q", rep.AgentVersion, a.Version)
	}
}

// A failed backup is reported as failed, and the status file must not lose the
// last known good snapshot.
func TestFailedBackupKeepsLastSuccess(t *testing.T) {
	gw := &fakeGateway{}
	rest := &fakeRestic{}
	a := newTestAgent(t, gw, rest)
	enrollTest(t, a)

	if _, err := a.BackupNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := a.store.Load()
	if err != nil {
		t.Fatal(err)
	}

	rest.err = &restic.RepoError{Op: "backup", Code: restic.ExitRepoLocked, Reason: "repository is locked"}
	if _, err := a.BackupNow(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	after, err := a.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if after.LastSuccess != first.LastSuccess {
		t.Error("a failed attempt must not move last_success: that is the whole point of tracking it")
	}
	if after.LastAttemptStatus != status.AttemptFailed {
		t.Errorf("last attempt = %q, want failed", after.LastAttemptStatus)
	}
	if after.ConsecutiveFailures != 1 {
		t.Errorf("consecutive_failures = %d, want 1", after.ConsecutiveFailures)
	}
	if len(gw.reports) != 2 || gw.reports[1].LastBackupStatus != "failed" {
		t.Errorf("reports = %+v, want the second to be a failure", gw.reports)
	}
	if gw.reports[1].LastBackupError == "" {
		t.Error("a failed report must say what went wrong")
	}
}

// A locked repository is exactly what retrying fixes.
func TestLockedRepositoryStaysRetryable(t *testing.T) {
	rest := &fakeRestic{err: &restic.RepoError{Op: "backup", Code: restic.ExitRepoLocked}}
	a := newTestAgent(t, &fakeGateway{}, rest)
	enrollTest(t, a)
	_, err := a.RunBackup(context.Background())
	if scheduler.IsTerminal(err) {
		t.Error("a locked repository is retryable and must not be terminal")
	}
}

// A missing repository must not be papered over by creating a new one: that
// would split the customer's history in two.
func TestMissingRepositoryIsTerminal(t *testing.T) {
	rest := &fakeRestic{err: &restic.RepoError{Op: "backup", Code: restic.ExitRepoMissing}}
	a := newTestAgent(t, &fakeGateway{}, rest)
	enrollTest(t, a)
	_, err := a.RunBackup(context.Background())
	if !scheduler.IsTerminal(err) {
		t.Error("a missing repository will not appear between retries: this needs a human")
	}
}

// With no folder configured anywhere, the agent must say so instead of silently
// backing up nothing or defaulting to a drive root.
func TestNoBackupPathIsTerminal(t *testing.T) {
	gw := &fakeGateway{config: &api.RemoteConfig{Status: api.StateActive}}
	rest := &fakeRestic{}
	a := newTestAgent(t, gw, rest)
	enrollTest(t, a)
	// Strip the gateway path so nothing tells the agent what to protect.
	a.cfg.Include = nil
	creds, err := a.cfg.SecretStore().Load()
	if err != nil {
		t.Fatal(err)
	}
	creds.BackupPath = ""
	if err := a.cfg.SecretStore().Save(creds); err != nil {
		t.Fatal(err)
	}

	_, err = a.RunBackup(context.Background())
	if err == nil || !scheduler.IsTerminal(err) {
		t.Fatalf("err = %v, want a terminal error about the missing folder", err)
	}
	if rest.backups != 0 {
		t.Error("restic must not run with nothing to back up")
	}
	if !strings.Contains(err.Error(), "no backup folder") {
		t.Errorf("error = %q, want it to name the missing configuration", err)
	}
}

// The status file is read by monitoring tools, so it must never contain a
// secret. This is the regression test for the 0.1.0 leak.
func TestStatusFileNeverContainsSecrets(t *testing.T) {
	a := newTestAgent(t, &fakeGateway{}, &fakeRestic{})
	enrollTest(t, a)
	if _, err := a.RunBackup(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(a.cfg.StatusFile)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	for _, secretValue := range []string{"encryption-key", "device-pass", "dev-01HQ8@", "password"} {
		if strings.Contains(body, secretValue) {
			t.Errorf("status.json contains %q:\n%s", secretValue, body)
		}
	}
	// The device id itself is fine and necessary; the repository URL is not.
	if strings.Contains(body, "rest:https://") {
		t.Errorf("status.json must identify the repository by host only:\n%s", body)
	}
}

// The device id is the only identity the agent may use, and a suspicious one is
// refused rather than sanitised into something that reaches a different path.
func TestImplausibleDeviceIDIsRejected(t *testing.T) {
	a := newTestAgent(t, &fakeGateway{}, &fakeRestic{})
	creds := &secret.Credentials{
		DeviceID:       "../../other-tenant",
		DevicePassword: "p",
		EncryptionKey:  "k",
		Repository:     "rest:https://backup.softafrique.net/x",
	}
	if err := a.secrets.Save(creds); err != nil {
		t.Fatal(err)
	}
	err := a.load()
	if err == nil {
		t.Fatal("a device id containing path separators must be refused")
	}
	if !strings.Contains(err.Error(), "re-enroll") {
		t.Errorf("error = %q, want it to say what to do", err)
	}
}

// The remote cache is what lets a device that boots with no network still know
// what to protect.
func TestRemoteCacheSurvivesAReboot(t *testing.T) {
	gw := &fakeGateway{config: &api.RemoteConfig{
		DeviceID:         "dev-01HQ8",
		Status:           api.StateActive,
		BackupPath:       customerDataDir(t),
		ScheduleInterval: api.Duration(time.Hour),
	}}
	a := newTestAgent(t, gw, &fakeRestic{})
	enrollTest(t, a)
	if err := a.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.RefreshConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.cfg.DataDir, "remote.json")); err != nil {
		t.Fatalf("the gateway configuration was not cached: %v", err)
	}

	// Now a fresh agent on the same data directory, with the gateway down.
	gw.configErr = &api.Error{Op: "config", Err: api.ErrUnavailable}
	restarted := newTestAgentIn(t, a.cfg.DataDir, gw, &fakeRestic{})
	if err := restarted.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	paths := restarted.backupPaths()
	if len(paths) != 1 || !strings.Contains(paths[0], "CustomerData") {
		t.Errorf("after a reboot with no network the agent should still know its folder, got %v", paths)
	}
}

// The 0.1.0 build kept the repository password in a plaintext file. Upgrading
// must remove it, and must not touch a file the operator put elsewhere.
func TestMigrationRemovesTheLegacyPasswordFile(t *testing.T) {
	a := newTestAgent(t, &fakeGateway{}, &fakeRestic{})
	enrollTest(t, a)

	inside := filepath.Join(a.cfg.DataDir, "restic-password")
	if err := os.WriteFile(inside, []byte("encryption-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "keepme")
	if err := os.WriteFile(outside, []byte("operator's own file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.cfg.PasswordFile = inside
	a.migrateLegacySecrets()
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Error("the legacy password file inside the data directory should have been removed")
	}

	a.cfg.PasswordFile = outside
	a.migrateLegacySecrets()
	if _, err := os.Stat(outside); err != nil {
		t.Error("a password file outside the data directory is the operator's business and must be left alone")
	}
}

// The heartbeat must not claim success on a device that has never succeeded.
func TestHeartbeatDoesNotClaimSuccessBeforeAnyBackup(t *testing.T) {
	gw := &fakeGateway{}
	a := newTestAgent(t, gw, &fakeRestic{})
	enrollTest(t, a)
	if err := a.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.reportHeartbeat(context.Background())
	if len(gw.reports) != 1 {
		t.Fatalf("sent %d reports, want 1", len(gw.reports))
	}
	if gw.reports[0].LastBackupStatus == "success" {
		t.Error("a device with no successful backup must not report success")
	}
	if gw.reports[0].LastBackupError == "" {
		t.Error("the report should say why")
	}
}

// A suspended device must not be reported to the gateway as a failed backup.
// Nothing was attempted, and last_backup_status is contractually only "success"
// or "failed", so the honest answer is to say nothing and let status.json carry
// the reason.
func TestSuspendedRunIsNotReportedAsAFailure(t *testing.T) {
	gw := &fakeGateway{config: &api.RemoteConfig{Status: api.StateActive, BackupPath: customerDataDir(t)}}
	a := newTestAgent(t, gw, &fakeRestic{})
	enrollTest(t, a)

	// One good run first, so there is a last success worth preserving.
	if _, err := a.BackupNow(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Now an operator suspends the device at the gateway.
	gw.config = &api.RemoteConfig{Status: api.StateSuspended, BackupPath: customerDataDir(t)}
	if _, err := a.BackupNow(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	for _, rep := range gw.reports {
		if rep.LastBackupStatus == "failed" {
			t.Errorf("reported a failed backup for a device that never tried: %+v", rep)
		}
	}
	st, err := a.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.LastAttemptStatus != status.AttemptSuspended {
		t.Errorf("last attempt = %q, want %q", st.LastAttemptStatus, status.AttemptSuspended)
	}
	if st.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d, want 0", st.ConsecutiveFailures)
	}
	if st.LastSuccess == "" {
		t.Error("last_success must be preserved across a suspension")
	}
}

// A heartbeat is what stops a device from looking stale on a dashboard, so it
// must never contradict the last real attempt. Reporting an old snapshot as a
// success while the device is suspended is the "always green" behaviour that
// finding A was about.
func TestHeartbeatDoesNotClaimSuccessWhileSuspended(t *testing.T) {
	gw := &fakeGateway{config: &api.RemoteConfig{Status: api.StateActive, BackupPath: customerDataDir(t)}}
	a := newTestAgent(t, gw, &fakeRestic{})
	enrollTest(t, a)

	if _, err := a.BackupNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := gw.reports[len(gw.reports)-1].LastBackupStatus; got != "success" {
		t.Fatalf("report after a good run = %q, want success", got)
	}

	gw.config = &api.RemoteConfig{Status: api.StateSuspended, BackupPath: customerDataDir(t)}
	if _, err := a.BackupNow(context.Background()); err == nil {
		t.Fatal("expected the suspended device to refuse")
	}
	gw.reports = nil
	a.reportHeartbeat(context.Background())

	rep := gw.reports[len(gw.reports)-1]
	if rep.LastBackupStatus == "success" {
		t.Errorf("heartbeat reported success for a suspended device: %+v", rep)
	}
	if !strings.Contains(rep.LastBackupError, "suspended") {
		t.Errorf("heartbeat error = %q, want the suspension reason", rep.LastBackupError)
	}
}
