// Package agent joins configuration, the gateway client, restic and status
// reporting into the thing that actually protects a customer's data.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"softafrique-backup-agent/internal/api"
	"softafrique-backup-agent/internal/config"
	"softafrique-backup-agent/internal/identity"
	"softafrique-backup-agent/internal/restic"
	"softafrique-backup-agent/internal/scheduler"
	"softafrique-backup-agent/internal/secret"
	"softafrique-backup-agent/internal/status"
)

// ErrNotEnrolled means the device has no credentials, so there is no repository
// to talk to and no identity to report as.
var ErrNotEnrolled = errors.New("this device is not enrolled: run 'agent enroll --token <token>'")

// deviceIDPattern is what the agent will accept as a device id. The value ends
// up as a path segment in the repository URL and as restic's --host, so
// anything that could change which repository is addressed is rejected rather
// than sanitised: a surprising id from the gateway is a gateway problem to fix,
// not something to paper over.
var deviceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// gateway is the part of the API client the agent uses.
type gateway interface {
	Config(ctx context.Context) (*api.RemoteConfig, error)
	PostStatus(ctx context.Context, report api.StatusReport) error
	Health(ctx context.Context) error
}

// backupRunner is the part of restic the agent uses.
type backupRunner interface {
	Ping(ctx context.Context) error
	Backup(ctx context.Context, include, exclude []string) (*restic.Result, error)
	Stats(ctx context.Context, mode string) (*restic.Stats, error)
}

// Agent ties together configuration, restic, the gateway and status reporting.
type Agent struct {
	cfg     *config.Config
	log     *slog.Logger
	store   *status.Store
	secrets *secret.Store
	cache   *remoteCache

	// Version is the agent build, reported to the gateway and the status file.
	// Injected by the CLI.
	Version string

	// override replaces the real gateway and restic clients. It is nil in
	// production and set only by this package's tests, which is the one place
	// that needs to drive a backup attempt without a repository.
	override *deps

	// mu guards the runtime state below, which the backup loop and the
	// background poller both touch.
	mu          sync.RWMutex
	creds       *secret.Credentials
	rest        backupRunner
	gw          gateway
	remote      *api.RemoteConfig
	suspended   bool
	suspendWhy  string
	gwState     string
	action      string
	configFails int
}

// New creates an Agent. Credentials and clients are loaded lazily so a command
// like `agent status` works on a device that has never enrolled.
func New(cfg *config.Config, store *status.Store, log *slog.Logger) *Agent {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Agent{
		cfg:     cfg,
		log:     log,
		store:   store,
		secrets: cfg.SecretStore(),
		cache:   newRemoteCache(filepath.Join(cfg.DataDir, "remote.json")),
	}
}

// deps carries injectable collaborators.
type deps struct {
	gw   gateway
	rest backupRunner
}

// withDeps returns a copy of a that uses the given collaborators instead of
// the real gateway and restic. Tests only.
func (a *Agent) withDeps(gw gateway, rest backupRunner) *Agent {
	a.override = &deps{gw: gw, rest: rest}
	return a
}

// Enrolled reports whether this device has credentials.
func (a *Agent) Enrolled() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.creds != nil
}

// DeviceID returns the enrollment-assigned device id, or "" when unenrolled.
func (a *Agent) DeviceID() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.creds == nil {
		return ""
	}
	return a.creds.DeviceID
}

// Config returns the live config, so a caller can see intervals the gateway has
// just changed.
func (a *Agent) Config() *config.Config { return a.cfg }

// load reads the credentials blob and builds the clients. It is safe to call
// repeatedly.
func (a *Agent) load() error {
	creds, err := a.secrets.Load()
	if err != nil {
		if errors.Is(err, secret.ErrNotFound) {
			a.mu.Lock()
			a.creds = nil
			a.mu.Unlock()
			return ErrNotEnrolled
		}
		return err
	}
	if !deviceIDPattern.MatchString(creds.DeviceID) {
		return fmt.Errorf("the stored device id %q is not a usable identifier; re-enroll the device", creds.DeviceID)
	}

	opts := restic.Options{
		Binary:     a.cfg.ResticPath,
		Repository: creds.Repository,
		Host:       creds.DeviceID,
		Password:   creds.ResticPassword(),
		Logger:     a.log,
	}
	if a.cfg.PasswordFile != "" && a.cfg.DeviceID != "" {
		// Only a hand-configured, unenrolled-style setup uses a password file.
		// An enrolled device always has the key in the blob.
		if _, statErr := os.Stat(a.cfg.PasswordFile); statErr == nil {
			opts.PasswordFile = a.cfg.PasswordFile
			opts.Password = ""
		}
	}

	server := creds.Server
	if server == "" {
		server = a.cfg.Server
	}
	client, err := api.New(server, creds.BasicAuthUser(), creds.DevicePassword, api.Options{
		UserAgent: "SoftafriqueBackupAgent/" + a.Version,
	})
	if err != nil {
		return err
	}

	if a.override != nil {
		a.mu.Lock()
		a.creds = creds
		if a.override.rest != nil {
			a.rest = a.override.rest
		}
		if a.override.gw != nil {
			a.gw = a.override.gw
		}
		a.mu.Unlock()
		a.writeIdentity()
		return nil
	}

	a.mu.Lock()
	a.creds = creds
	a.rest = restic.New(opts)
	a.gw = client
	// Restore the last known remote configuration so a device that boots with no
	// network still knows its backup folder and schedule.
	cached, _ := a.cache.Load()
	a.remote = cached
	if cached != nil {
		a.applyRemoteLocked(cached)
	}
	a.mu.Unlock()

	a.writeIdentity()
	return nil
}

// writeIdentity records the non-secret identity fields in the status file so a
// monitoring script can tell two devices apart.
func (a *Agent) writeIdentity() {
	creds := a.credentials()
	info := identity.Collect()
	paths := a.backupPaths()
	repoHost := ""
	if creds != nil {
		repoHost = hostOf(creds.Repository)
	}
	_ = a.store.Update(func(st *status.Status) {
		tenant := ""
		if creds != nil {
			tenant = creds.Tenant
		}
		st.SetIdentity(credsID(creds), tenant, info.Hostname, info.OSCaption, a.Version, repoHost)
		st.SetBackupPaths(paths)
		st.Enrolled = creds != nil
	})
}

func credsID(c *secret.Credentials) string {
	if c == nil {
		return ""
	}
	return c.DeviceID
}

func (a *Agent) credentials() *secret.Credentials {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.creds
}

func (a *Agent) resticRunner() backupRunner {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.rest
}

func (a *Agent) gatewayClient() gateway {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.gw
}

// hostOf extracts the host from a restic repository URL for display.
func hostOf(raw string) string {
	s := secret.SanitizeURL(raw)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/:"); i >= 0 {
		s = s[:i]
	}
	return s
}

// backupPaths resolves what to back up, in order of precedence:
//
//  1. an explicit include in config.yaml, which is the operator's deliberate
//     override and wins over everything;
//  2. backup_path from GET /config, the gateway's current instruction;
//  3. backup_path from the POST /enroll reply, so a device that enrolled while
//     the gateway is unreachable, or that cannot reach it now, still knows what
//     it was enrolled to protect.
//
// Empty is possible and is an error, never a reason to default to a drive root.
func (a *Agent) backupPaths() []string {
	a.mu.RLock()
	var serverPath, enrolledPath string
	if a.remote != nil {
		serverPath = a.remote.BackupPath
	}
	if a.creds != nil {
		enrolledPath = a.creds.BackupPath
	}
	a.mu.RUnlock()
	return a.cfg.BackupPaths(firstNonEmpty(serverPath, enrolledPath))
}

// Suspension returns whether backups are currently withheld and why.
func (a *Agent) Suspension() (bool, string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.suspended, a.suspendWhy
}

// Prepare loads credentials, checks the environment and performs the 0.1.0
// migration. It returns ErrNotEnrolled on a device that has never enrolled.
func (a *Agent) Prepare(ctx context.Context) error {
	if err := a.load(); err != nil {
		return err
	}
	a.migrateLegacySecrets()

	if warn := a.cfg.DataDirACLWarning(); warn != "" {
		a.log.Warn("data directory is not the installed location", "detail", warn)
	}
	if info := identity.Collect(); info.OSCaption == "" || strings.EqualFold(info.OSCaption, "Windows") {
		a.log.Warn("could not determine the exact operating system; os_caption will be imprecise")
	}
	return nil
}

// migrateLegacySecrets removes the 0.1.0 plaintext password file.
//
// The 0.1.0 build had to be handed the repository password in a file, or typed
// into the repository URL. Now that the key is in the DPAPI blob, leaving those
// behind would leave the secret on disk in the clear, which is the exact finding
// that started this work.
func (a *Agent) migrateLegacySecrets() {
	if a.cfg.PasswordFile == "" {
		return
	}
	if _, err := os.Stat(a.cfg.PasswordFile); err != nil {
		return
	}
	// Only remove it if it is inside our own data directory: an operator may
	// legitimately keep a password file somewhere else and reference it on
	// purpose.
	inside := strings.HasPrefix(strings.ToLower(filepath.Clean(a.cfg.PasswordFile)),
		strings.ToLower(filepath.Clean(a.cfg.DataDir)+string(filepath.Separator)))
	if !inside {
		return
	}
	if err := os.Remove(a.cfg.PasswordFile); err != nil {
		a.log.Warn("could not remove the legacy plaintext password file", "path", a.cfg.PasswordFile, "err", err)
		return
	}
	a.log.Info("removed the legacy plaintext password file; the key is now held in the encrypted store",
		"path", a.cfg.PasswordFile)
}

// SelfTest verifies the repository is reachable and the credentials work.
func (a *Agent) SelfTest(ctx context.Context) error {
	if err := a.load(); err != nil {
		return err
	}
	runner := a.resticRunner()
	if err := runner.Ping(ctx); err != nil {
		return fmt.Errorf("repository self-test failed: %w", err)
	}
	if gw := a.gatewayClient(); gw != nil {
		if err := gw.Health(ctx); err != nil {
			// Not fatal: the gateway API and the repository endpoint are
			// different services, and being unable to reach the API must not
			// stop the backup.
			a.log.Warn("gateway health check did not answer", "err", err)
		}
	}
	return nil
}

// RunBackup performs one backup attempt and reports the outcome. It is the
// scheduler's RunFunc, so it does not touch the status file: the scheduler
// records the attempt it asked for.
func (a *Agent) RunBackup(ctx context.Context) (status.Outcome, error) {
	outcome, err := a.backupOnce(ctx)
	a.reportStatus(ctx, outcome)
	return outcome, err
}

// BackupNow runs exactly one backup, records it and returns what happened.
// It backs the `agent backup` command.
func (a *Agent) BackupNow(ctx context.Context) (status.Outcome, error) {
	sched := scheduler.New(a.cfg, a.store, a.RunBackup, a.log)
	return sched.RunOnce(ctx)
}

// backupOnce is a single attempt, without reporting.
func (a *Agent) backupOnce(ctx context.Context) (status.Outcome, error) {
	start := time.Now()

	if err := a.load(); err != nil {
		out := status.Outcome{Elapsed: time.Since(start), Skipped: true, Err: err}
		if errors.Is(err, ErrNotEnrolled) {
			return out, scheduler.Terminal(err)
		}
		return out, err
	}

	// Refresh the remote configuration first. A failure here is not fatal: the
	// last known configuration stands, because data protection must not depend
	// on the control plane being reachable.
	a.RefreshConfig(ctx)

	if suspended, why := a.Suspension(); suspended {
		a.markSuspended(why)
		out := status.Outcome{
			Elapsed: time.Since(start),
			Skipped: true,
			Err:     errors.New("backup withheld: " + why),
		}
		return out, scheduler.Terminal(out.Err)
	}

	paths := a.backupPaths()
	if len(paths) == 0 {
		err := errors.New("no backup folder is configured: set include in config.yaml or let the gateway report one")
		return status.Outcome{Elapsed: time.Since(start), Skipped: true, Err: err}, scheduler.Terminal(err)
	}

	// A path the agent will not protect is refused here, before restic is given
	// it, because that is the only point at which the agent is still the one
	// deciding.
	//
	// The installer applies the same rule to what a technician typed. This catches
	// what the gateway said instead, in a backup_path on /config or an enrollment
	// reply, and it is deliberately terminal: retrying every five minutes against
	// a share the machine account cannot read produces the same failure forever
	// and buries it.
	if refused := a.refusedPaths(paths); len(refused) > 0 {
		err := refusalError(refused)
		a.log.Error("backup withheld", "err", err)
		return status.Outcome{Elapsed: time.Since(start), Skipped: true, Err: err}, scheduler.Terminal(err)
	}

	// The folder to protect is created if it has gone missing.
	//
	// Before this, a folder deleted after installation meant a device that
	// reported a failed backup every hour, forever, until a human noticed. That
	// is the worst of both worlds: the customer is not protected, and the only
	// symptom is a red dashboard nobody reads.
	//
	// Recreating it and carrying on is right, but reporting a plain success
	// would be a lie -- the snapshot would hold an empty directory. The paths
	// created are carried into the Outcome so it is recorded as
	// status.AttemptRecreated, which keeps last_success where it really was.
	created := a.ensureBackupPaths(paths)

	runner := a.resticRunner()
	a.log.Info("starting backup", "paths", paths, "device_id", a.DeviceID())

	res, err := runner.Backup(ctx, paths, a.cfg.Exclude)
	outcome := status.Outcome{Elapsed: time.Since(start), RecreatedPaths: created}
	if err != nil {
		outcome.Err = err
		return outcome, a.classifyBackupError(err)
	}
	outcome.SnapshotID = res.SnapshotID
	outcome.FilesAdded = res.FilesAdded
	outcome.FilesChanged = res.FilesChanged
	outcome.BytesAdded = res.BytesAdded
	outcome.ResticDuration = res.ResticDuration

	a.log.Info("backup complete",
		"snapshot", res.SnapshotID,
		"files_added", res.FilesAdded,
		"files_changed", res.FilesChanged,
		"bytes_added", res.BytesAdded,
		"elapsed", outcome.Elapsed.Round(time.Second),
		"restic_duration", res.ResticDuration.Round(time.Second),
	)
	return outcome, nil
}

// classifyBackupError decides whether a failure is worth retrying.
//
// A missing repository or a bad password will not fix itself between attempts
// five minutes apart, so those are terminal and the agent waits for a human. A
// network failure or a locked repository is exactly what the retry loop is for.
func (a *Agent) classifyBackupError(err error) error {
	var repoErr *restic.RepoError
	if errors.As(err, &repoErr) {
		switch repoErr.Code {
		case restic.ExitRepoMissing, restic.ExitRepoConfigUnreadable:
			return scheduler.Terminal(err)
		}
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return err
}

// markSuspended records that no attempt was made and why.
//
// It uses the gateway state the agent already knows, so a device whose
// credentials were rejected is recorded as "revoked" rather than being flattened
// into the same "suspended" as one an operator paused. The dashboard tells those
// two apart: one needs a new token, the other needs a decision.
func (a *Agent) markSuspended(why string) {
	a.mu.RLock()
	state := a.gwState
	a.mu.RUnlock()
	if state == "" {
		state = "suspended"
	}
	_ = a.store.Update(func(st *status.Status) {
		st.MarkSuspended(time.Now(), why)
		st.SetGatewayState(state, why)
	})
}

// reportStatus posts the outcome to the gateway. A reporting failure never
// fails the backup, but a rejection is remembered because it means the gateway
// no longer considers this device enrolled.
func (a *Agent) reportStatus(ctx context.Context, outcome status.Outcome) {
	gw := a.gatewayClient()
	if gw == nil {
		return
	}
	if outcome.Skipped {
		// Nothing was attempted, so there is no attempt to report. The gateway
		// already knows it suspended this device, and last_backup_status is
		// contractually only "success" or "failed"; reporting either would be a
		// lie. The local status file carries the reason.
		a.log.Info("not reporting a status for a run that made no attempt",
			"reason", errString(outcome.Err))
		return
	}
	st, err := a.store.Load()
	if err != nil {
		a.log.Warn("could not read status before reporting", "err", err)
		return
	}
	rep := api.StatusReport{
		LastBackupStatus:    "failed",
		LastSnapshotID:      st.LastSnapshotID,
		FilesAdded:          st.FilesAdded,
		FilesChanged:        st.FilesChanged,
		BytesAdded:          st.BytesAdded,
		LastDurationSeconds: st.LastDurationSeconds,
		AgentVersion:        a.Version,
		OSCaption:           st.OSCaption,
	}
	if outcome.Err == nil && len(outcome.RecreatedPaths) == 0 {
		rep.LastBackupStatus = "success"
		if outcome.SnapshotID != "" {
			rep.LastSnapshotID = outcome.SnapshotID
			rep.FilesAdded = outcome.FilesAdded
			rep.FilesChanged = outcome.FilesChanged
			rep.BytesAdded = outcome.BytesAdded
		}
		rep.LastDurationSeconds = outcome.Elapsed.Seconds()
	} else if outcome.Err != nil {
		rep.LastBackupError = outcome.Err.Error()
	} else {
		// The attempt did not error, but the only reason it could run is that
		// the agent created the folder itself, so the snapshot holds an empty
		// directory. Reporting "success" here is how a device stops protecting a
		// customer's data without the dashboard noticing, so the contract's
		// two values leave "failed" with the reason as the honest answer.
		rep.LastBackupError = "the protected folder was missing and had to be recreated, " +
			"so this backup captured an empty folder: " + strings.Join(outcome.RecreatedPaths, ", ")
	}

	// A long backup must not leave the report hanging on the shutdown path.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := gw.PostStatus(ctx, rep); err != nil {
		a.noteGatewayRejection(err)
		a.log.Warn("could not report status to the gateway", "err", err)
		return
	}
	a.clearAction()
}

// noteGatewayRejection reacts to the gateway refusing this device.
func (a *Agent) noteGatewayRejection(err error) {
	var action string
	switch {
	case errors.Is(err, api.ErrUnauthorized):
		action = "re-enroll: the gateway no longer accepts this device's credentials"
	case errors.Is(err, api.ErrRevoked):
		action = "re-enroll: the gateway has withdrawn this device"
	}
	if action == "" {
		return
	}
	a.mu.Lock()
	already := a.action == action && a.suspended
	a.action = action
	a.suspended = true
	a.suspendWhy = action
	a.gwState = "revoked"
	a.mu.Unlock()
	if !already {
		a.log.Error("gateway rejected this device; backups are paused", "action", action)
	}
	_ = a.store.Update(func(st *status.Status) { st.SetGatewayState("revoked", action) })
}

// clearAction drops a pending "action required" once the gateway has accepted
// the device again.
//
// It deliberately does not touch ServerStatus: a successful POST /status proves
// the credentials work, but it says nothing about whether an operator has since
// suspended the device, and overwriting that would make status.json claim a
// suspended device is active.
func (a *Agent) clearAction() {
	a.mu.Lock()
	had := a.action != ""
	a.action = ""
	suspended := a.suspended
	a.mu.Unlock()
	if !had || suspended {
		return
	}
	_ = a.store.Update(func(st *status.Status) { st.ActionRequired = "" })
}

// RefreshConfig fetches GET /config and applies it.
//
// On a transport failure or a 5xx the previous configuration is kept and the
// agent carries on backing up. Only a successful reply that says the device is
// not active stops backups, because that is a deliberate act by an operator
// rather than a network hiccup.
func (a *Agent) RefreshConfig(ctx context.Context) error {
	gw := a.gatewayClient()
	if gw == nil {
		return ErrNotEnrolled
	}
	remote, err := gw.Config(ctx)
	if err != nil {
		if errors.Is(err, api.ErrUnauthorized) || errors.Is(err, api.ErrRevoked) {
			a.noteGatewayRejection(err)
			return err
		}
		a.mu.Lock()
		a.configFails++
		n := a.configFails
		a.mu.Unlock()
		if n == 1 || n%12 == 0 {
			a.log.Warn("could not refresh the gateway configuration; continuing with the last known values",
				"consecutive_failures", n, "err", err)
		}
		return err
	}

	a.mu.Lock()
	a.configFails = 0
	a.mu.Unlock()

	a.applyRemote(remote)
	if err := a.cache.Save(remote); err != nil {
		a.log.Warn("could not cache the gateway configuration", "err", err)
	}
	return nil
}

// applyRemoteLocked adopts a remote configuration. The caller holds a.mu.
func (a *Agent) applyRemoteLocked(remote *api.RemoteConfig) {
	a.remote = remote
	if d := remote.ScheduleInterval.Duration(); d > 0 {
		a.cfg.ScheduleInterval = d
	}
	if d := remote.RetryInterval.Duration(); d > 0 {
		a.cfg.RetryInterval = d
	}
	if !remote.IsActive() {
		a.suspended = true
		a.suspendWhy = remote.SuspendReason()
		a.gwState = string(remote.Status)
	} else {
		// The gateway has authoritatively said this device may back up, so a
		// stale suspension from an earlier rejection is over.
		a.suspended = false
		a.suspendWhy = ""
		a.action = ""
		a.gwState = string(remote.Status)
	}
}

func (a *Agent) applyRemote(remote *api.RemoteConfig) {
	a.mu.Lock()
	a.applyRemoteLocked(remote)
	a.mu.Unlock()

	state := string(remote.Status)
	if state == "" {
		state = "unknown"
	}
	action := ""
	if a.suspended {
		action = a.suspendWhy
	}
	_ = a.store.Update(func(st *status.Status) {
		st.SetGatewayState(state, action)
		st.SetBackupPaths(a.backupPaths())
		st.SetScheduleInterval(a.cfg.ScheduleInterval)
		if a.action == "" {
			st.ActionRequired = ""
		}
	})
}

// RefreshStats takes a repository usage reading for the monitoring file. It is
// throttled by the caller because it is a round trip to the gateway.
func (a *Agent) RefreshStats(ctx context.Context) {
	if !a.cfg.Stats() {
		return
	}
	runner := a.resticRunner()
	if runner == nil {
		return
	}
	raw, err := runner.Stats(ctx, restic.ModeRawData)
	if err != nil {
		a.log.Debug("repository usage unavailable", "err", err)
		return
	}
	restore, err := runner.Stats(ctx, restic.ModeRestoreSize)
	if err != nil {
		a.log.Debug("restore size unavailable", "err", err)
	}
	entry := &status.RepoStats{
		RepoBytes:     raw.TotalSize,
		FileCount:     raw.TotalFileCount,
		SnapshotCount: raw.SnapshotsCount,
		CapturedAt:    status.Now(),
	}
	if restore != nil {
		entry.RestoreBytes = restore.TotalSize
	}
	_ = a.store.Update(func(st *status.Status) { st.RepoStats = entry })
}

// RunScheduled runs the scheduled loop until ctx is cancelled. It assumes the
// single-instance lock is already held by the caller.
func (a *Agent) RunScheduled(ctx context.Context) error {
	if err := a.Prepare(ctx); err != nil {
		return err
	}
	// Refresh before the first attempt so a newly installed device picks up its
	// gateway-assigned folder without a manual run.
	_ = a.RefreshConfig(ctx)

	go a.background(ctx)

	sched := scheduler.New(a.cfg, a.store, a.RunBackup, a.log)
	return sched.Run(ctx)
}

// background refreshes the gateway configuration, reports a heartbeat and takes
// a usage reading while the agent is idle.
func (a *Agent) background(ctx context.Context) {
	statsEvery := 30 * time.Minute
	tick := a.cfg.ConfigPollInterval
	if a.cfg.StatusHeartbeat < tick {
		tick = a.cfg.StatusHeartbeat
	}
	if statsEvery < tick {
		tick = statsEvery
	}
	if tick <= 0 {
		tick = time.Minute
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	lastConfig := time.Now()
	lastHeartbeat := time.Now()
	lastStats := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			if now.Sub(lastConfig) >= a.cfg.ConfigPollInterval {
				lastConfig = now
				_ = a.RefreshConfig(ctx)
				a.writeIdentity()
			}
			if now.Sub(lastHeartbeat) >= a.cfg.StatusHeartbeat {
				lastHeartbeat = now
				// An idle device still reports, so "online and healthy" is
				// distinguishable from "offline" in the dashboard.
				if suspended, why := a.Suspension(); suspended {
					a.markSuspended(why)
				}
				a.reportHeartbeat(ctx)
			}
			if now.Sub(lastStats) >= statsEvery && a.cfg.Stats() {
				lastStats = now
				a.RefreshStats(ctx)
			}
		}
	}
}

// reportHeartbeat posts the current state with no new attempt.
func (a *Agent) reportHeartbeat(ctx context.Context) {
	gw := a.gatewayClient()
	if gw == nil {
		return
	}
	st, err := a.store.Load()
	if err != nil {
		return
	}
	rep := api.StatusReport{
		LastBackupStatus:    "success",
		LastSnapshotID:      st.LastSnapshotID,
		FilesAdded:          st.FilesAdded,
		FilesChanged:        st.FilesChanged,
		BytesAdded:          st.BytesAdded,
		LastDurationSeconds: st.LastDurationSeconds,
		AgentVersion:        a.Version,
		OSCaption:           st.OSCaption,
	}
	switch {
	case st.LastSnapshotID == "":
		// Nothing has ever succeeded, so "success" would be a lie.
		rep.LastBackupStatus = "failed"
		rep.LastBackupError = "no successful backup has completed on this device yet"
	case !st.IsOperational():
		// A heartbeat is what keeps a device from looking stale on a dashboard.
		// It must never contradict the last real attempt: reporting the old
		// snapshot as a success while the device is suspended or awaiting a
		// decision is the "always green" behaviour that finding A was about.
		reason := st.ActionRequired
		if reason == "" {
			reason = st.LastAttemptError
		}
		rep.LastBackupStatus = "failed"
		rep.LastBackupError = reason
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := gw.PostStatus(ctx, rep); err != nil {
		a.noteGatewayRejection(err)
		a.log.Warn("heartbeat report failed", "err", err)
	}
}

// Restore restores a snapshot into target.
func (a *Agent) Restore(ctx context.Context, snapshot, target string, include []string) error {
	if err := a.load(); err != nil {
		return err
	}
	runner, ok := a.resticRunner().(interface {
		Restore(ctx context.Context, snapshot, target string, include []string) error
	})
	if !ok {
		return errors.New("restic runner does not support restore")
	}
	a.log.Info("restoring snapshot", "snapshot", snapshot, "target", target, "include", include)
	return runner.Restore(ctx, snapshot, target, include)
}

// LatestSnapshot returns the most recent snapshot, or nil.
func (a *Agent) LatestSnapshot(ctx context.Context) (*restic.Snapshot, error) {
	if err := a.load(); err != nil {
		return nil, err
	}
	runner, ok := a.resticRunner().(interface {
		LatestSnapshot(ctx context.Context) (*restic.Snapshot, error)
	})
	if !ok {
		return nil, errors.New("restic runner does not support snapshot listing")
	}
	return runner.LatestSnapshot(ctx)
}
