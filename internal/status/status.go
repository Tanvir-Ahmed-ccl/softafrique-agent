// Package status maintains the machine-readable state file that Tactical RMM
// and dashboards read.
//
// Design rules for this file:
//
//   - It is read by a monitoring system, so it must never contain a secret.
//     There is no password field and no repository URL, only RepoHost. The
//     redaction test in this package is what keeps it that way.
//   - It is written by two goroutines (the backup loop and the reporting
//     heartbeat), so all mutation goes through Store.Update, which holds a lock
//     across the read-modify-write. Loading and saving separately loses updates.
//   - Every timestamp is UTC in RFC3339. agent.log uses the same convention;
//     see docs/OPERATIONS.md.
package status

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SchemaVersion identifies the layout. Bumped from 1 (0.1.0) to 2 with the
// gateway integration; a reader that does not check it should tolerate extra
// fields.
const SchemaVersion = 2

// Attempt outcomes recorded in LastAttemptStatus.
const (
	// AttemptRunning means an attempt has started and not finished.
	AttemptRunning = "running"
	// AttemptSuccess means the last attempt completed and produced a snapshot.
	AttemptSuccess = "success"
	// AttemptFailed means the last attempt ended in an error.
	AttemptFailed = "failed"
	// AttemptSuspended means the gateway withdrew the device, so no attempt was
	// made. Distinct from failed: retrying will not help until an operator acts.
	AttemptSuspended = "suspended"
	// AttemptRecreated means the attempt succeeded, but only because the agent
	// had to create the folder it was asked to protect. The snapshot therefore
	// holds an empty directory and nothing else.
	//
	// It is a success in the narrow sense that nothing errored, and a lie in
	// every sense that matters: reporting this as a plain success is how a device
	// whose customer deleted their data folder reads as protected on a dashboard
	// while protecting nothing. It is reported distinctly so the staleness of
	// last_success still catches it.
	AttemptRecreated = "recreated"
)

// recreateNoticePrefix marks an ActionRequired that this package wrote, so a
// later clean run can clear its own notice without touching a "re-enroll" one
// left by the gateway. The prefix is part of the value a monitoring script
// reads, so it is short and stable.
const recreateNoticePrefix = "the protected folder was missing"

// RepoStats is the most recent repository usage reading.
type RepoStats struct {
	// RepoBytes is how much space the data occupies in the repository
	// (restic stats --mode raw-data).
	RepoBytes uint64 `json:"repo_bytes"`
	// RestoreBytes is the logical size of the data as restored
	// (restic stats --mode restore-size).
	RestoreBytes uint64 `json:"restore_bytes"`
	// FileCount and SnapshotCount come from the same calls.
	FileCount     uint64 `json:"file_count"`
	SnapshotCount uint64 `json:"snapshot_count"`
	// CapturedAt is when these numbers were read, so a dashboard can tell a
	// stale reading from a fresh one.
	CapturedAt string `json:"captured_at"`
}

// Status is the state file's contents.
//
// There is no field here that can hold a credential, by construction. The
// repository is identified by host only: a full URL is a place a password can
// end up, which is exactly how the 0.1.0 build leaked one into this file.
type Status struct {
	// SchemaVersion is the layout version of this document.
	SchemaVersion int `json:"schema_version"`

	// ---- identity (never secret) ----

	// DeviceID is the enrollment-assigned device id, the same value passed to
	// restic --host. It is never derived from the machine name.
	DeviceID string `json:"device_id"`
	// Tenant is the customer this device belongs to.
	Tenant string `json:"tenant"`
	// Hostname is the Windows computer name, kept for display next to DeviceID
	// so an operator can tell "dev-01HQ8" on DESKTOP-ABC from the same id on a
	// re-imaged machine.
	Hostname     string `json:"hostname"`
	OSCaption    string `json:"os_caption"`
	AgentVersion string `json:"agent_version"`

	// ---- what is protected, and where it lives ----

	// BackupPaths are the folders handed to restic.
	BackupPaths []string `json:"backup_paths"`
	// RepoHost is the repository's host only, e.g. "backup.softafrique.net".
	RepoHost string `json:"repo_host"`

	// ---- gateway view ----

	// Enrolled is false on a device that has no credentials blob, which is the
	// state a monitoring check should alert on.
	Enrolled bool `json:"enrolled"`
	// ServerStatus is the last status the gateway reported: active, suspended,
	// or unknown when the gateway could not be reached.
	ServerStatus string `json:"server_status"`
	// ActionRequired is a short instruction for a human when the agent cannot
	// proceed on its own, e.g. "re-enroll: the gateway rejected the device
	// credentials". Empty when nothing is needed.
	ActionRequired string `json:"action_required,omitempty"`

	// ---- attempts ----

	// LastAttemptStart and LastAttemptEnd bracket the most recent attempt.
	// LastAttemptStatus is its outcome. A device that has never run has an empty
	// LastAttemptStatus, which is different from "running" and from "failed".
	LastAttemptStart  string `json:"last_attempt_start"`
	LastAttemptEnd    string `json:"last_attempt_end"`
	LastAttemptStatus string `json:"last_attempt_status"`
	LastAttemptError  string `json:"last_attempt_error"`

	// LastDurationSeconds is elapsed wall-clock time for the last attempt,
	// including any time spent waiting for the network. restic's own
	// total_duration excludes that wait, so it is reported separately as
	// ResticDurationSeconds: a backup that waited three and a half minutes for
	// the network and then worked for one second must not be logged as one
	// second.
	LastDurationSeconds   float64 `json:"last_duration_seconds"`
	ResticDurationSeconds float64 `json:"restic_duration_seconds"`

	// ---- last success, which is not the same as last attempt ----

	// LastSuccess is when a backup last completed successfully. It is empty
	// until the first one does, and it does not move when an attempt fails, so
	// "how long since we were last actually protected" is answerable.
	LastSuccess    string `json:"last_success"`
	LastSnapshotID string `json:"last_snapshot_id"`
	FilesAdded     uint64 `json:"files_added"`
	FilesChanged   uint64 `json:"files_changed"`
	BytesAdded     uint64 `json:"bytes_added"`
	BackupCount    uint64 `json:"backup_count"`
	// ConsecutiveFailures drives the health check's "failing" state.
	ConsecutiveFailures uint64 `json:"consecutive_failures"`

	// RepoStats is the most recent usage reading, absent until one is taken.
	RepoStats *RepoStats `json:"repo_stats,omitempty"`

	// NextBackupTime is when the agent intends to run again. It is a plan, not a
	// promise: a retry after a failure happens sooner.
	NextBackupTime string `json:"next_backup_time"`

	// ScheduleIntervalSeconds is the effective interval between successful
	// backups, after the gateway's schedule_interval has been applied. A
	// monitoring script needs it to decide when "no backup since" has become
	// "not backed up": a machine on a 15-minute schedule and one on a 24-hour
	// schedule cannot share a single staleness threshold, and hard-coding one
	// either alerts on a healthy fleet or misses a dead device.
	ScheduleIntervalSeconds float64 `json:"schedule_interval_seconds"`

	// ---- 0.1.0 compatibility ----

	// LastBackupStatus and LastBackupTime are kept because monitoring scripts
	// written against 0.1.0 read them. LastBackupStatus mirrors
	// LastAttemptStatus and LastBackupTime mirrors LastSuccess. They will be
	// dropped in a future major version, not silently changed.
	LastBackupStatus string `json:"last_backup_status"`
	LastBackupTime   string `json:"last_backup_time"`

	// UpdatedAt is when this document was written, UTC RFC3339.
	UpdatedAt string `json:"updated_at"`
}

// Stamp formats a time the way every timestamp in this package is formatted.
func Stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// Now returns the current time as a stamp.
func Now() string { return Stamp(time.Now()) }

// ParseStamp reads a stamp written by Stamp.
func ParseStamp(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// New returns an empty, versioned status.
func New() *Status { return &Status{SchemaVersion: SchemaVersion} }

// SetIdentity records the non-secret identity fields. It does not touch attempt
// or success state.
func (s *Status) SetIdentity(deviceID, tenant, hostname, osCaption, agentVersion, repoHost string) {
	s.DeviceID = deviceID
	s.Tenant = tenant
	s.Hostname = hostname
	s.OSCaption = osCaption
	s.AgentVersion = agentVersion
	s.RepoHost = repoHost
	s.Enrolled = deviceID != ""
}

// SetBackupPaths records what is being protected.
func (s *Status) SetBackupPaths(paths []string) {
	s.BackupPaths = append([]string(nil), paths...)
}

// SetGatewayState records what the gateway last said.
func (s *Status) SetGatewayState(state, actionRequired string) {
	s.ServerStatus = state
	s.ActionRequired = actionRequired
}

// MarkRunning records the start of an attempt. It deliberately leaves
// LastSuccess alone: an attempt in progress has not protected anything yet.
func (s *Status) MarkRunning(start time.Time) {
	s.LastAttemptStart = Stamp(start)
	s.LastAttemptEnd = ""
	s.LastAttemptStatus = AttemptRunning
	s.LastAttemptError = ""
	s.LastBackupStatus = AttemptRunning
}

// Outcome describes a finished attempt.
type Outcome struct {
	// SnapshotID is the snapshot produced, empty on failure.
	SnapshotID string
	// FilesAdded, FilesChanged and BytesAdded come from restic's summary.
	FilesAdded   uint64
	FilesChanged uint64
	BytesAdded   uint64
	// Elapsed is the wall-clock time the attempt took, including network wait.
	Elapsed time.Duration
	// ResticDuration is restic's own total_duration.
	ResticDuration time.Duration
	// Skipped is true when no backup was attempted at all, because the device
	// is suspended, unenrolled or has nothing configured. A skipped run must not
	// be reported to the gateway as a failed backup: nothing was tried, and
	// reporting a failure would make a deliberately suspended device look
	// broken.
	Skipped bool
	// RecreatedPaths are folders the agent had to create because they had gone
	// missing since the last run. Non-empty on an otherwise successful attempt
	// means the snapshot protected an empty directory, which is recorded as
	// AttemptRecreated rather than as a success.
	RecreatedPaths []string
	// Err is the failure, nil on success.
	Err error
}

// MarkFinished records the end of an attempt and updates the success fields.
//
// On success LastSuccess, the counters and the snapshot id all move. On failure
// only the attempt fields and ConsecutiveFailures move, so a device that has
// been failing for a week still shows when it was last actually protected.
func (s *Status) MarkFinished(end time.Time, out Outcome) {
	s.LastAttemptEnd = Stamp(end)
	s.LastDurationSeconds = out.Elapsed.Seconds()
	s.ResticDurationSeconds = out.ResticDuration.Seconds()

	// A skipped run is not a failure. A suspended device, an unenrolled device
	// and a device with nothing configured all produce an error, but nothing was
	// attempted, so counting them as failures would fill a monitoring dashboard
	// with red for a decommissioned machine and make the failure counter
	// meaningless on the machines that matter.
	if out.Skipped {
		s.LastAttemptStatus = AttemptSuspended
		s.LastAttemptError = errText(out.Err)
		s.LastBackupStatus = AttemptSuspended
		return
	}

	s.LastBackupStatus = AttemptFailed

	if out.Err == nil {
		// The 0.1.0 compatibility mirror has to be moved on the success path
		// too. Leaving it at "failed" makes every monitoring script written
		// against 0.1.0 report a healthy device as broken, which is the same
		// class of bug as the 0.1.0 statuses themselves.
		s.LastBackupStatus = AttemptSuccess
	}

	if out.Err != nil {
		s.LastAttemptStatus = AttemptFailed
		s.LastAttemptError = errText(out.Err)
		s.ConsecutiveFailures++
		return
	}

	// The agent had to create the folder it was asked to protect, so the
	// snapshot that just succeeded holds an empty directory.
	//
	// This deliberately does NOT move LastSuccess, LastSnapshotID or the file
	// counters. Those fields answer "when was this device last actually
	// protected", and a snapshot of a directory the agent created thirty seconds
	// earlier did not protect anything. Leaving them alone is what makes the
	// health check's staleness threshold eventually fire and say so.
	//
	// It also deliberately does not increment ConsecutiveFailures. Nothing failed,
	// and filling that counter with a non-failure would make it useless for the
	// machines it exists to catch.
	if len(out.RecreatedPaths) > 0 {
		s.LastAttemptStatus = AttemptRecreated
		s.LastAttemptError = recreateNotice(out.RecreatedPaths)
		// The 0.1.0 compatibility mirror reads last_backup_status and nothing
		// else. A 0.1.0-era monitor must not see "success" here.
		s.LastBackupStatus = AttemptFailed
		if s.ActionRequired == "" || isRecreateNotice(s.ActionRequired) {
			s.ActionRequired = s.LastAttemptError
		}
		return
	}

	s.LastAttemptStatus = AttemptSuccess
	s.LastAttemptError = ""
	s.ConsecutiveFailures = 0
	s.LastSuccess = Stamp(end)
	s.LastBackupTime = s.LastSuccess
	s.LastSnapshotID = out.SnapshotID
	s.FilesAdded = out.FilesAdded
	s.FilesChanged = out.FilesChanged
	s.BytesAdded = out.BytesAdded
	s.BackupCount++
	// A clean run clears our own notice, and only ours: a pending "re-enroll"
	// from the gateway is not ours to delete, and that one is cleared by
	// clearAction once the gateway accepts the device again.
	if isRecreateNotice(s.ActionRequired) {
		s.ActionRequired = ""
	}
}

// recreateNotice is the operator-facing text for a recreated folder. It names
// the paths, because "something is wrong" is not actionable at 2am and a path is
// the one thing that can be checked without a site visit.
//
// The path is quoted with plain double quotes rather than %q, because %q escapes
// the backslashes in a Windows path and the reader then has to work out whether
// they are real.
func recreateNotice(paths []string) string {
	quoted := make([]string, 0, len(paths))
	for _, p := range paths {
		quoted = append(quoted, `"`+p+`"`)
	}
	if len(quoted) == 1 {
		return fmt.Sprintf("%s and has been created: %s. The backup that followed protected an empty folder, "+
			"so confirm the customer's data is there", recreateNoticePrefix, quoted[0])
	}
	return fmt.Sprintf("%s and has been created: %s. The backup that followed protected empty folders, "+
		"so confirm the customer's data is there", recreateNoticePrefix, strings.Join(quoted, ", "))
}

func isRecreateNotice(action string) bool {
	return strings.HasPrefix(action, recreateNoticePrefix)
}

// errText renders an outcome error for the status file, tolerating a nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// MarkSuspended records that the gateway withdrew the device, so no attempt was
// made. It is not a failure: ConsecutiveFailures is untouched because nothing
// went wrong locally, and last success is preserved.
func (s *Status) MarkSuspended(at time.Time, reason string) {
	s.LastAttemptStatus = AttemptSuspended
	s.LastAttemptEnd = Stamp(at)
	s.LastBackupStatus = AttemptSuspended
	s.LastAttemptError = reason
	s.SetGatewayState(string(AttemptSuspended), reason)
}

// SetScheduleInterval records the effective interval between backups.
func (s *Status) SetScheduleInterval(d time.Duration) {
	s.ScheduleIntervalSeconds = d.Seconds()
}

// SetNextBackup records when the agent plans to run next. An empty interval
// clears it.
func (s *Status) SetNextBackup(at time.Time) {
	if at.IsZero() {
		s.NextBackupTime = ""
		return
	}
	s.NextBackupTime = Stamp(at)
}

// Age returns how long ago t was, for a health check.
func Age(stamp string, now time.Time) (time.Duration, bool) {
	t, ok := ParseStamp(stamp)
	if !ok {
		return 0, false
	}
	d := now.UTC().Sub(t)
	if d < 0 {
		return 0, true
	}
	return d, true
}

// Clone returns a deep copy, so a caller holding a status cannot mutate the
// store's copy.
func (s *Status) Clone() *Status {
	out := *s
	if s.BackupPaths != nil {
		out.BackupPaths = append([]string(nil), s.BackupPaths...)
	}
	if s.RepoStats != nil {
		stats := *s.RepoStats
		out.RepoStats = &stats
	}
	return &out
}

// Store persists Status to a JSON file.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore returns a Store writing to path.
func NewStore(path string) *Store { return &Store{path: path} }

// Path is the file location, for logs and error messages.
func (s *Store) Path() string { return s.path }

// Load reads the status, returning a fresh versioned status when the file does
// not exist yet.
func (s *Store) Load() (*Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() (*Status, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return New(), nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return New(), nil
	}
	// Unmarshal into a zero value, not New(): a 0.1.0 file has no
	// schema_version field, and pre-setting it would make an old file look
	// current and skip the migration.
	st := &Status{}
	if err := json.Unmarshal(data, st); err != nil {
		// A corrupt status file must not stop the agent from backing up, but the
		// caller should know it happened, so the error is returned alongside a
		// usable object.
		return New(), errors.New("status file was unreadable and has been reset: " + err.Error())
	}
	if st.SchemaVersion < SchemaVersion {
		migrateFromV1(st)
	}
	return st, nil
}

// migrateFromV1 lifts a 0.1.0 status file into the current layout.
//
// Two things happen. The schedule-relevant timestamp is carried across so a
// 0.2.0 upgrade does not trigger an immediate redundant backup. And the old
// `repo` field, which is where 0.1.0 wrote the credential-bearing repository
// URL, is simply not a field any more: json ignores it on read and the next
// write drops it, so the upgrade is what removes that credential from disk.
func migrateFromV1(st *Status) {
	if st.LastSuccess == "" && st.LastBackupTime != "" && st.LastBackupStatus == AttemptSuccess {
		st.LastSuccess = st.LastBackupTime
	}
	if st.LastAttemptStatus == "" && st.LastBackupStatus != "" {
		st.LastAttemptStatus = st.LastBackupStatus
	}
	if st.LastBackupTime == "" {
		st.LastBackupTime = st.LastSuccess
	}
	st.LastBackupStatus = st.LastAttemptStatus
	st.SchemaVersion = SchemaVersion
}

// Save writes st atomically: a crash mid-write leaves the previous file intact
// rather than a half-written one that a monitoring script would choke on.
func (s *Store) Save(st *Status) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(st)
}

func (s *Store) saveLocked(st *Status) error {
	st.SchemaVersion = SchemaVersion
	st.UpdatedAt = Now()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Update applies fn to the stored status and saves the result, atomically with
// respect to other callers. The callback must not retain the pointer.
func (s *Store) Update(fn func(*Status)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadLocked()
	if err != nil {
		// A corrupt file is replaced rather than propagated: the agent's job is
		// to keep backing up, and the next write repairs the file.
		st = New()
	}
	if fn != nil {
		fn(st)
	}
	return s.saveLocked(st)
}

// IsOperational reports whether the last attempt on this device was a real one
// that succeeded, with nothing outstanding that needs a human. It is the
// single question a dashboard and a heartbeat both need answered, so they cannot
// disagree.
func (s *Status) IsOperational() bool {
	return s.LastAttemptStatus == AttemptSuccess &&
		s.LastBackupStatus == AttemptSuccess &&
		s.ServerStatus == "active" &&
		s.ActionRequired == ""
}
