// Package restic wraps the restic binary.
//
// Two rules shape everything here:
//
//  1. The repository password never appears in a command line. restic accepts
//     it in the URL, in --password-file or in the environment; the environment
//     is the only one of the three that is not readable by every local user
//     through the process table. The repository URL is therefore expected to be
//     credential-free, and secret.Credentials.Validate rejects one that is not.
//
//  2. The gateway is append-only. Nothing in this package creates a forget or
//     prune call, and TestArgsNeverContainRetentionCommands asserts it, so the
//     guarantee Mohsin verified in the gateway logs is a property of the code
//     rather than a promise.
package restic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// restic exit codes the agent reacts to. The restic manual reserves 10-19 for
// repository level failures and 20+ for source data problems.
const (
	// ExitRepoMissing means "the repository does not exist". This is the only
	// condition under which the agent will run `restic init`.
	ExitRepoMissing = 10
	// ExitRepoConfigUnreadable means the repository exists but its config could
	// not be opened, i.e. the password is wrong or the data is damaged.
	ExitRepoConfigUnreadable = 11
	// ExitRepoLocked means another restic process holds the repository lock.
	ExitRepoLocked = 13
	// ExitSourceError means some source files could not be read.
	ExitSourceError = 20
)

// stats modes.
const (
	// ModeRestoreSize is the logical size of the data as restored.
	ModeRestoreSize = "restore-size"
	// ModeRawData is the size the data occupies in the repository.
	ModeRawData = "raw-data"
)

// Result summarizes a completed restic backup run.
type Result struct {
	SnapshotID   string
	FilesAdded   uint64
	FilesChanged uint64
	BytesAdded   uint64
	// ResticDuration is restic's own total_duration: how long restic spent
	// working. It excludes however long the run waited on the network before
	// restic got going, so it is not the number to show a customer or put on a
	// dashboard. The agent measures elapsed wall-clock time separately.
	ResticDuration time.Duration
}

// Stats is a subset of `restic stats --json`.
type Stats struct {
	Mode            string `json:"mode"`
	TotalSize       uint64 `json:"total_size"`
	TotalFileCount  uint64 `json:"total_file_count"`
	SnapshotsCount  uint64 `json:"snapshots_count"`
	SourceDataAdded uint64 `json:"source_data_added"`
}

// Snapshot is a single restic snapshot.
type Snapshot struct {
	ID    string    `json:"id"`
	Time  time.Time `json:"time"`
	Host  string    `json:"hostname"`
	Paths []string  `json:"paths"`
}

// Options configures a Restic.
type Options struct {
	// Binary is the restic executable path.
	Binary string
	// Repository is the restic repository URL. It must not contain credentials.
	Repository string
	// Host is the value passed to restic --host. It is the enrollment-assigned
	// device id, and it must be the same value the gateway knows the device by,
	// otherwise central reporting cannot attribute a snapshot to a device.
	Host string
	// Password is the repository password. It is held in memory only and handed
	// to restic through the child process environment.
	Password string
	// PasswordFile overrides Password with restic's --password-file. This exists
	// for manual development and break-glass recovery; production devices are
	// enrolled and use Password.
	PasswordFile string
	// Logger receives restic's own output. It may be nil.
	Logger *slog.Logger
}

// Restic wraps the restic binary.
type Restic struct {
	opts Options
	log  *slog.Logger
}

// New builds a Restic from opts.
func New(opts Options) *Restic {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Restic{opts: opts, log: log}
}

// Host is the --host value, for logging.
func (r *Restic) Host() string { return r.opts.Host }

// Repository is the repository URL. It is safe to log: Validate rejects a URL
// with credentials in it, and Repository itself does not redact.
func (r *Restic) Repository() string { return r.opts.Repository }

// args builds the global flag set. Every restic invocation in this package goes
// through it, which is what makes TestArgsNeverContainRetentionCommands
// meaningful.
func (r *Restic) args(extra ...string) []string {
	base := []string{"-r", r.opts.Repository}
	if r.opts.Host != "" {
		base = append(base, "--host", r.opts.Host)
	}
	if r.opts.PasswordFile != "" {
		base = append(base, "--password-file", r.opts.PasswordFile)
	}
	return append(base, extra...)
}

// newCmd prepares a restic invocation.
//
// The password is injected as RESTIC_PASSWORD in the child's environment. restic
// reads --password-file first, then RESTIC_PASSWORD_FILE, then
// RESTIC_PASSWORD, so setting the variable is enough and nothing has to be
// written to disk to hand restic a secret.
func (r *Restic) newCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, r.opts.Binary, args...)
	cmd.Env = os.Environ()
	if r.opts.PasswordFile == "" && r.opts.Password != "" {
		cmd.Env = append(cmd.Env, "RESTIC_PASSWORD="+r.opts.Password)
	}
	// A predictable cache location keeps restic's state out of the service
	// account's roaming profile, and keeps two invocations from fighting over
	// the default %LOCALAPPDATA% path when one runs as SYSTEM and one as an
	// operator.
	if dir := r.cacheDir(); dir != "" {
		cmd.Env = append(cmd.Env, "RESTIC_CACHE_DIR="+dir)
		cmd.Env = append(cmd.Env, "TMPDIR="+dir)
	}
	return cmd
}

func (r *Restic) cacheDir() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	dir := filepath.Join(base, "SoftafriqueBackupAgent", "restic-cache")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	return dir
}

// RedactedRepository stands in for the repository URL in any text the agent
// prints.
//
// The URL is not a credential, but it carries the device id, and both the status
// contract and the documentation promise it stays out of the log. restic echoes
// the repository in its own errors, so the substitution happens where that
// output enters the program rather than at each print site.
const RedactedRepository = "[repository]"

// stderrSink captures restic's output for the log and keeps a tail of it for the
// error message.
//
// The 0.1.0 build pointed cmd.Stderr at os.Stderr, which is discarded when the
// agent runs as a service under LocalSystem: restic's actual complaint never
// reached agent.log, which is why field failures were hard to diagnose.
type stderrSink struct {
	log  *slog.Logger
	tail *tailBuffer
	repo string
	mu   sync.Mutex
	// pend holds the bytes of a line that has not been terminated yet, so a
	// message split across two writes is logged once, not twice.
	pend []byte
}

func newStderrSink(log *slog.Logger, repo string) *stderrSink {
	return &stderrSink{log: log, tail: newTailBuffer(8 << 10), repo: repo}
}

func (s *stderrSink) Write(p []byte) (int, error) {
	s.tail.Write(s.redact(p))

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pend = append(s.pend, p...)
	for {
		i := bytes.IndexByte(s.pend, '\n')
		if i < 0 {
			break
		}
		if line := strings.TrimSpace(string(s.pend[:i])); line != "" {
			s.log.Debug("restic", "line", line)
		}
		s.pend = s.pend[i+1:]
	}
	// A progress line that never terminates would otherwise grow without bound.
	if len(s.pend) > 64<<10 {
		if line := strings.TrimSpace(string(s.pend)); line != "" {
			s.log.Debug("restic", "line", line)
		}
		s.pend = s.pend[:0]
	}
	return len(p), nil
}

// Tail is the last few kilobytes of restic's output.
func (s *stderrSink) Tail() string { return s.tail.String() }

// redact removes the repository URL from restic's output.
//
// restic echoes the repository in its own error messages, and those messages
// reach agent.log, the console and the RMM task output. The status report
// deliberately omits the repository, and the documentation promises the same for
// the log, so the URL is replaced here at the one place restic output enters the
// program. Everything an operator needs to diagnose the failure is left in
// place: restic names the exit code and the reason, and the URL is the only
// thing removed.
func (s *stderrSink) redact(p []byte) []byte {
	if s.repo == "" {
		return p
	}
	out := bytes.ReplaceAll(p, []byte(s.repo), []byte(RedactedRepository))
	// restic also prints the repository with a trailing slash, and the tail of
	// the URL on its own, so cover the base name as well.
	if i := strings.LastIndexByte(s.repo, '/'); i > 0 {
		out = bytes.ReplaceAll(out, []byte(s.repo[i:]), []byte(RedactedRepository))
	}
	return out
}

// tailBuffer keeps only the last n bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		// Keep the tail: the interesting line is the last one restic printed.
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// ExitCode returns restic's exit code and whether the error was an exit at all
// (as opposed to the binary being missing or the context being cancelled).
func ExitCode(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), true
	}
	return 0, false
}

// IsRepoMissing reports whether err means the repository does not exist yet.
func IsRepoMissing(err error) bool {
	code, ok := ExitCode(err)
	return ok && code == ExitRepoMissing
}

// IsRepoLocked reports whether err means another process holds the repository.
func IsRepoLocked(err error) bool {
	code, ok := ExitCode(err)
	return ok && code == ExitRepoLocked
}

// RepoError describes why a repository operation failed, in terms the agent can
// act on.
type RepoError struct {
	Op     string
	Code   int
	Reason string
	Detail string
}

func (e *RepoError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "restic %s failed (exit %d): %s", e.Op, e.Code, e.Reason)
	if e.Detail != "" {
		fmt.Fprintf(&b, ": %s", e.Detail)
	}
	return b.String()
}

// Reason maps a restic exit code to something an operator can act on.
func Reason(code int) string {
	switch code {
	case ExitRepoMissing:
		return "the repository does not exist at this path"
	case ExitRepoConfigUnreadable:
		return "the repository exists but could not be opened; the password is wrong or the repository is damaged"
	case ExitRepoLocked:
		return "another restic process holds the repository lock"
	case ExitSourceError:
		return "some source files could not be read"
	default:
		return "restic reported an error"
	}
}

func repoErr(op string, err error, sink *stderrSink) error {
	code, ok := ExitCode(err)
	if !ok {
		// The binary is missing or the context was cancelled. Not a repository
		// problem and, importantly, not a reason to try `init`.
		return fmt.Errorf("restic %s: %w", op, err)
	}
	detail := ""
	if sink != nil {
		detail = sink.Tail()
	}
	return &RepoError{Op: op, Code: code, Reason: Reason(code), Detail: detail}
}

// EnsureRepo makes sure the repository is usable, and never creates one.
//
// In 0.1.0 this ran `restic init` after *any* failure to list snapshots, which
// meant a wrong device id created a brand new empty repository at the wrong path
// -- splitting the customer's history in two -- and a rejected password was
// reported as "repo unreachable and init failed". The fix went through two
// stages: init only on restic's exit code 10, and only when a config file asked
// for it. Neither stage is needed now, because the gateway creates the
// repository server-side at enrollment and fails the enrollment if restic init
// does not succeed. So there is no init code path left here at all.
//
// A missing repository is therefore a fact about the world, not something to
// work around: it is reported, and the agent stops. TestNoInitCodePathExists
// keeps that true if this function is ever tempted back open.
func (r *Restic) EnsureRepo(ctx context.Context) error {
	err := r.Ping(ctx)
	if err == nil {
		return nil
	}
	if IsRepoMissing(err) {
		return &RepoError{
			Op:     "check repository",
			Code:   ExitRepoMissing,
			Reason: Reason(ExitRepoMissing) + "; the gateway creates it at enrollment, so this is a gateway or enrollment problem, not something to fix by initialising",
		}
	}
	return err
}

// Ping verifies the repository is reachable and the password works.
func (r *Restic) Ping(ctx context.Context) error {
	sink := newStderrSink(r.log, r.opts.Repository)
	cmd := r.newCmd(ctx, r.args("--json", "snapshots", "--latest", "1")...)
	cmd.Stdout = io.Discard
	cmd.Stderr = sink
	if err := cmd.Run(); err != nil {
		return repoErr("snapshots", err, sink)
	}
	return nil
}

// Backup runs a restic backup and returns the summary restic emitted.
func (r *Restic) Backup(ctx context.Context, include, exclude []string) (*Result, error) {
	args := r.args("--json", "backup")
	args = append(args, normalizedPaths(include)...)
	for _, e := range exclude {
		if e != "" {
			args = append(args, "--exclude", e)
		}
	}

	sink := newStderrSink(r.log, r.opts.Repository)
	cmd := r.newCmd(ctx, args...)
	cmd.Stderr = sink
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start restic backup: %w", err)
	}

	res := &Result{}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 128*1024), 8*1024*1024)
	for sc.Scan() {
		// A malformed progress line is not worth failing a backup over: the
		// summary at the end is what matters.
		_ = parseBackupLine(sc.Bytes(), res)
	}
	scanErr := sc.Err()
	if err := cmd.Wait(); err != nil {
		return nil, repoErr("backup", err, sink)
	}
	if scanErr != nil {
		return nil, fmt.Errorf("restic backup: reading output: %w", scanErr)
	}
	if res.SnapshotID == "" {
		return nil, fmt.Errorf("restic backup: no summary produced")
	}
	return res, nil
}

// parseBackupLine decodes one JSON line of restic backup progress and keeps the
// final summary in res.
func parseBackupLine(line []byte, res *Result) error {
	var msg struct {
		MessageType   string  `json:"message_type"`
		SnapshotID    string  `json:"snapshot_id"`
		FilesNew      uint64  `json:"files_new"`
		FilesChanged  uint64  `json:"files_changed"`
		BytesAdded    uint64  `json:"bytes_added"`
		DataAdded     uint64  `json:"data_added"`
		TotalDuration float64 `json:"total_duration"`
		Status        string  `json:"status"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil // non-JSON noise, e.g. warnings
	}
	if msg.MessageType != "summary" {
		return nil
	}
	if msg.Status != "" && msg.Status != "ok" {
		return fmt.Errorf("restic backup reported status %q", msg.Status)
	}
	if msg.SnapshotID == "" {
		return nil // a progress line; wait for the real summary
	}
	res.SnapshotID = msg.SnapshotID
	res.FilesAdded = msg.FilesNew
	res.FilesChanged = msg.FilesChanged
	// restic >= 0.17 reports data_added; older versions report bytes_added.
	if msg.BytesAdded != 0 {
		res.BytesAdded = msg.BytesAdded
	} else {
		res.BytesAdded = msg.DataAdded
	}
	res.ResticDuration = time.Duration(msg.TotalDuration * float64(time.Second))
	return nil
}

// LatestSnapshot returns the most recent snapshot, or nil when the repository
// has none yet.
func (r *Restic) LatestSnapshot(ctx context.Context) (*Snapshot, error) {
	sink := newStderrSink(r.log, r.opts.Repository)
	cmd := r.newCmd(ctx, r.args("--json", "snapshots", "--latest", "1")...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = sink
	if err := cmd.Run(); err != nil {
		return nil, repoErr("snapshots", err, sink)
	}
	trimmed := strings.TrimSpace(out.String())
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var snaps []Snapshot
	if err := json.Unmarshal(out.Bytes(), &snaps); err != nil {
		return nil, fmt.Errorf("restic snapshots: invalid JSON: %w", err)
	}
	if len(snaps) == 0 {
		return nil, nil
	}
	return &snaps[0], nil
}

// Stats returns repository usage. It reads snapshot metadata only, so it is
// cheap on a small repository, but it is still a round trip to the gateway and
// is therefore called on a throttle rather than after every backup.
func (r *Restic) Stats(ctx context.Context, mode string) (*Stats, error) {
	if mode != ModeRestoreSize && mode != ModeRawData {
		return nil, fmt.Errorf("unknown stats mode %q", mode)
	}
	sink := newStderrSink(r.log, r.opts.Repository)
	cmd := r.newCmd(ctx, r.args("--json", "stats", "--mode", mode)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = sink
	if err := cmd.Run(); err != nil {
		return nil, repoErr("stats", err, sink)
	}
	stats := &Stats{Mode: mode}
	if err := json.Unmarshal(out.Bytes(), stats); err != nil {
		return nil, fmt.Errorf("restic stats: invalid JSON: %w", err)
	}
	return stats, nil
}

// Restore restores snapshot ("latest" allowed) into target, optionally filtered
// by include patterns.
func (r *Restic) Restore(ctx context.Context, snapshot, target string, include []string) error {
	args := r.args("restore", snapshot, "--target", target)
	for _, in := range include {
		if in != "" {
			args = append(args, "--include", in)
		}
	}
	sink := newStderrSink(r.log, r.opts.Repository)
	cmd := r.newCmd(ctx, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = sink
	if err := cmd.Run(); err != nil {
		return repoErr("restore", err, sink)
	}
	return nil
}

// normalizedPaths strips trailing separators so restic reports stable path
// labels, and keeps the separator on a bare Windows drive root.
func normalizedPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		clean := strings.TrimRight(p, `\/`)
		if clean == "" {
			clean = p
		}
		if len(clean) == 2 && clean[1] == ':' && runtime.GOOS == "windows" {
			clean += string(filepath.Separator)
		}
		out = append(out, clean)
	}
	return out
}
