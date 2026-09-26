package status

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	secretPassword = "9f2c4a7b1d8e"
	secretKey      = "e3b1f0a95c2d47e6b8a10f34c9d2e7b56"
)

func newPopulated() *Status {
	st := New()
	st.SetIdentity("dev-01HQ8XK3M4N7", "softafrique", "MOHSIN", "Windows 11 Pro", "0.2.0", "backup.softafrique.net")
	st.SetBackupPaths([]string{`C:\SoftafriqueBackup`})
	st.SetGatewayState("active", "")
	return st
}

// TestMarshalledStatusContainsNoSecret is the test behind finding 1: the file
// that Tactical RMM reads must not carry the device password or the encryption
// key, in any field, under any name.
func TestMarshalledStatusContainsNoSecret(t *testing.T) {
	st := newPopulated()
	st.MarkRunning(time.Now())
	st.MarkFinished(time.Now(), Outcome{
		SnapshotID: "abc123",
		BytesAdded: 4512,
		Elapsed:    213 * time.Second,
	})
	st.RepoStats = &RepoStats{RepoBytes: 1 << 30, RestoreBytes: 2 << 30, CapturedAt: Now()}

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, secret := range []string{secretPassword, secretKey, "password", "encryption_key", "device_password", "@"} {
		if strings.Contains(body, secret) {
			t.Errorf("status file leaks %q:\n%s", secret, body)
		}
	}
	if !strings.Contains(body, "backup.softafrique.net") {
		t.Error("repo host should still be reported")
	}
	if strings.Contains(body, "rest:https://") {
		t.Error("status file should not contain a repository URL, only its host")
	}
}

// TestNoCredentialFieldExists is the structural half of the same guarantee: even
// before anyone knows what the secrets are, the schema has nowhere to put them.
func TestNoCredentialFieldExists(t *testing.T) {
	typ := reflect.TypeOf(Status{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		if strings.Contains(name, "password") || strings.Contains(name, "secret") ||
			strings.Contains(name, "key") || strings.Contains(name, "token") {
			t.Errorf("Status has a field that looks like a credential: %s", typ.Field(i).Name)
		}
	}
}

func TestAttemptVersusSuccessAreDistinct(t *testing.T) {
	st := newPopulated()
	first := time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)

	// A success.
	st.MarkRunning(first)
	st.MarkFinished(first.Add(30*time.Second), Outcome{
		SnapshotID: "snap1", FilesChanged: 2, BytesAdded: 4512,
		Elapsed: 30 * time.Second, ResticDuration: 1 * time.Second,
	})
	if st.LastSuccess != Stamp(first.Add(30*time.Second)) {
		t.Errorf("last_success = %q", st.LastSuccess)
	}
	if st.LastDurationSeconds != 30 {
		t.Errorf("elapsed duration = %v, want 30 (the elapsed time, not restic's 1s)", st.LastDurationSeconds)
	}
	if st.ResticDurationSeconds != 1 {
		t.Errorf("restic duration = %v, want 1 (kept separately)", st.ResticDurationSeconds)
	}

	// Now a failure. last_success must not move, and the attempt fields must.
	good := st.LastSuccess
	failedAt := first.Add(2 * time.Hour)
	st.MarkRunning(failedAt)
	st.MarkFinished(failedAt.Add(10*time.Second), Outcome{Err: errors.New("network unreachable"), Elapsed: 10 * time.Second})

	if st.LastSuccess != good {
		t.Errorf("last_success moved on failure: %q -> %q", good, st.LastSuccess)
	}
	if st.LastAttemptStatus != AttemptFailed {
		t.Errorf("last attempt status = %q, want failed", st.LastAttemptStatus)
	}
	if st.LastAttemptError == "" {
		t.Error("the failure reason should be recorded")
	}
	if st.ConsecutiveFailures != 1 {
		t.Errorf("consecutive_failures = %d, want 1", st.ConsecutiveFailures)
	}
	if st.LastDurationSeconds != 10 {
		t.Errorf("a failed attempt should still report how long it took, got %v", st.LastDurationSeconds)
	}
	// The 0.1.0 aliases must track the new fields.
	if st.LastBackupStatus != AttemptFailed {
		t.Errorf("last_backup_status alias = %q, want failed", st.LastBackupStatus)
	}
	if st.LastBackupTime != good {
		t.Errorf("last_backup_time alias = %q, want the last success %q", st.LastBackupTime, good)
	}
}

func TestSuspendedIsNotAFailure(t *testing.T) {
	st := newPopulated()
	at := time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)
	st.MarkRunning(at)
	st.MarkFinished(at, Outcome{SnapshotID: "snap1", Elapsed: time.Second})
	st.MarkSuspended(at.Add(time.Hour), "gateway reports device status \"suspended\"")

	if st.LastAttemptStatus != AttemptSuspended {
		t.Errorf("status = %q, want suspended", st.LastAttemptStatus)
	}
	if st.ConsecutiveFailures != 0 {
		t.Errorf("a suspension is not a local failure; consecutive_failures = %d", st.ConsecutiveFailures)
	}
	if st.LastSuccess == "" {
		t.Error("the last real success must be preserved through a suspension")
	}
	if st.ActionRequired == "" {
		t.Error("a suspension should tell the operator what to do")
	}
}

func TestNeverRunIsNotRunningOrFailed(t *testing.T) {
	st := newPopulated()
	if st.LastAttemptStatus != "" || st.LastSuccess != "" || st.LastAttemptStart != "" {
		t.Errorf("a device that has never run should have empty attempt fields: %+v", st)
	}
	// A monitoring script must be able to tell "never ran" from "running now".
	age, ok := Age(st.LastSuccess, time.Now())
	if ok {
		t.Error("an empty stamp must not parse as a time")
	}
	if age != 0 {
		t.Error("age of an empty stamp should be zero")
	}
}

func TestStoreUpdateIsAtomicAcrossGoroutines(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "status.json"))
	var wg sync.WaitGroup
	// Two writers, as in production: the backup loop and the reporting
	// heartbeat. Without a lock across read-modify-write, updates are lost.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Update(func(st *Status) { st.ConsecutiveFailures++ })
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Update(func(st *Status) { st.BackupCount++ })
		}()
	}
	wg.Wait()

	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.ConsecutiveFailures != 20 {
		t.Errorf("consecutive_failures = %d, want 20: concurrent updates were lost", st.ConsecutiveFailures)
	}
	if st.BackupCount != 20 {
		t.Errorf("backup_count = %d, want 20: concurrent updates were lost", st.BackupCount)
	}
	if st.UpdatedAt == "" {
		t.Error("updated_at should always be stamped")
	}
}

func TestStoreRecoversFromCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewStore(path)
	if _, err := s.Load(); err == nil {
		t.Error("Load should report that the file was unreadable")
	}
	// The agent must still be able to record state afterwards.
	if err := s.Update(func(st *Status) { st.LastAttemptStatus = AttemptSuccess }); err != nil {
		t.Fatal(err)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.LastAttemptStatus != AttemptSuccess {
		t.Errorf("status not repaired, got %q", st.LastAttemptStatus)
	}
}

func TestStoreUpgradesSchemaV1File(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	// A 0.1.0 status file.
	legacy := `{"device_id":"mohsin","repo":"rest:https://user:hunter2@backup.softafrique.net/repos/mohsin","last_backup_time":"2026-09-20T08:00:00Z","last_backup_status":"success"}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewStore(path)
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", st.SchemaVersion, SchemaVersion)
	}
	// The legacy file's last success is carried into the new field so the
	// schedule resumes where it left off instead of re-running immediately.
	if st.LastSuccess != "2026-09-20T08:00:00Z" {
		t.Errorf("last_success = %q, want the value carried over from last_backup_time", st.LastSuccess)
	}
	// And the legacy URL is not carried forward, so the rewrite drops the
	// credential that 0.1.0 wrote into this file.
	if err := s.Update(func(*Status) {}); err != nil {
		t.Fatal(err)
	}
	rewritten, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rewritten), "hunter2") {
		t.Errorf("the credential in the legacy repo field survived the rewrite:\n%s", rewritten)
	}
}

func TestAge(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	if _, ok := Age("", now); ok {
		t.Error("empty stamp should not parse")
	}
	if _, ok := Age("nonsense", now); ok {
		t.Error("garbage should not parse")
	}
	d, ok := Age(Stamp(now.Add(-90*time.Minute)), now)
	if !ok || d != 90*time.Minute {
		t.Errorf("Age = %v (ok=%v), want 1h30m", d, ok)
	}
	future, ok := Age(Stamp(now.Add(time.Hour)), now)
	if !ok || future != 0 {
		t.Errorf("a clock skew must not produce a negative age, got %v", future)
	}
}

func TestCloneIsDeep(t *testing.T) {
	st := newPopulated()
	st.RepoStats = &RepoStats{RepoBytes: 10}
	c := st.Clone()
	c.BackupPaths[0] = "D:\\Other"
	c.RepoStats.RepoBytes = 99
	if st.BackupPaths[0] == "D:\\Other" {
		t.Error("Clone shares the paths slice")
	}
	if st.RepoStats.RepoBytes == 99 {
		t.Error("Clone shares the repo stats pointer")
	}
}

// The 0.1.0 compatibility mirror has to agree with the attempt fields. A
// monitoring script written against 0.1.0 reads last_backup_status, and if that
// mirror is stale the dashboard calls a healthy device broken.
func TestLegacyMirrorFollowsSuccess(t *testing.T) {
	s := New()
	s.MarkFinished(time.Now(), Outcome{SnapshotID: "abc", Elapsed: time.Second})
	if s.LastAttemptStatus != AttemptSuccess {
		t.Fatalf("attempt status = %q", s.LastAttemptStatus)
	}
	if s.LastBackupStatus != AttemptSuccess {
		t.Errorf("last_backup_status = %q, want %q: the 0.1.0 mirror must follow a success",
			s.LastBackupStatus, AttemptSuccess)
	}
	if s.LastBackupTime != s.LastSuccess {
		t.Errorf("last_backup_time = %q, want it to mirror last_success %q",
			s.LastBackupTime, s.LastSuccess)
	}
}

func TestLegacyMirrorFollowsFailure(t *testing.T) {
	s := New()
	s.MarkFinished(time.Now(), Outcome{SnapshotID: "abc"})
	s.MarkFinished(time.Now(), Outcome{Err: errors.New("boom")})
	if s.LastBackupStatus != AttemptFailed {
		t.Errorf("last_backup_status = %q, want %q", s.LastBackupStatus, AttemptFailed)
	}
	if s.LastBackupTime != s.LastSuccess {
		t.Error("a failure must not move the 0.1.0 last_backup_time either: it is the mirror of last_success")
	}
}

// The effective interval has to be visible so a monitor can scale its staleness
// threshold instead of hard-coding one for every device.
func TestSetScheduleInterval(t *testing.T) {
	s := New()
	s.SetScheduleInterval(90 * time.Minute)
	if s.ScheduleIntervalSeconds != 5400 {
		t.Errorf("schedule_interval_seconds = %v, want 5400", s.ScheduleIntervalSeconds)
	}
}

// A run that never happened must not be recorded as a failure. Otherwise a
// decommissioned device fills a dashboard with red and the failure counter on
// the machines that matter stops meaning anything.
func TestSkippedRunIsNotAFailure(t *testing.T) {
	s := New()
	s.MarkFinished(time.Now(), Outcome{SnapshotID: "abc"})
	lastSuccess := s.LastSuccess

	s.MarkFinished(time.Now(), Outcome{
		Skipped: true,
		Err:     errors.New(`backup withheld: gateway reports device status "suspended"`),
	})
	if s.LastAttemptStatus != AttemptSuspended {
		t.Errorf("last attempt = %q, want %q", s.LastAttemptStatus, AttemptSuspended)
	}
	if s.LastBackupStatus != AttemptSuspended {
		t.Errorf("legacy mirror = %q, want %q", s.LastBackupStatus, AttemptSuspended)
	}
	if s.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d, want 0: nothing went wrong locally", s.ConsecutiveFailures)
	}
	if s.LastSuccess != lastSuccess {
		t.Error("a skipped run must not move last_success")
	}
	if !strings.Contains(s.LastAttemptError, "suspended") {
		t.Errorf("last_attempt_error = %q, want the reason an operator needs", s.LastAttemptError)
	}
}
