package restic

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"softafrique-backup-agent/internal/config"
)

// Result summarizes a completed restic backup run.
type Result struct {
	SnapshotID    string
	FilesAdded    uint64
	FilesChanged  uint64
	BytesAdded    uint64
	TotalDuration time.Duration
}

// Snapshot is a single restic snapshot used for status/inspection.
type Snapshot struct {
	ID      string    `json:"id"`
	Time    time.Time `json:"time"`
	Host    string    `json:"hostname"`
	Paths   []string  `json:"paths"`
	Summary struct {
		FilesNew uint64 `json:"files_new"`
		BytesNew uint64 `json:"bytes_added"`
	} `json:"summary"`
}

// Restic wraps the restic binary.
type Restic struct {
	path         string
	repo         string
	passwordFile string
}

// New builds a Restic wrapper from config.
func New(cfg *config.Config) *Restic {
	return &Restic{
		path:         cfg.ResticPath,
		repo:         cfg.Repo,
		passwordFile: cfg.PasswordFile,
	}
}

func (r *Restic) global() []string {
	return []string{"-r", r.repo, "--password-file", r.passwordFile}
}

// EnsureRepo verifies the repository is reachable/initialized, creating it via
// `restic init` if it does not exist yet (so a fresh install works without a
// pre-provisioned repo).
func (r *Restic) EnsureRepo(ctx context.Context) error {
	if _, err := runCaptureOutput(ctx, r.path, append(r.global(), "snapshots", "--latest", "1")...); err == nil {
		return nil
	}
	out, initErr := runCaptureOutput(ctx, r.path, append(r.global(), "init")...)
	if initErr != nil {
		return fmt.Errorf("ensure repo: repo unreachable and init failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// Ping verifies connectivity to the repo by listing the latest snapshot.
func (r *Restic) Ping(ctx context.Context) error {
	args := append(r.global(), "--json", "snapshots", "--latest", "1")
	return runCapture(ctx, r.path, args...)
}

// Backup runs a restic backup of the include paths with JSON progress and
// returns the summary emitted by restic.
func (r *Restic) Backup(ctx context.Context, include, exclude []string) (*Result, error) {
	args := append(r.global(), "--json", "backup")
	args = append(args, normalizedPaths(include)...)
	if len(exclude) > 0 {
		for _, e := range exclude {
			args = append(args, "--exclude", e)
		}
	}

	cmd := exec.CommandContext(ctx, r.path, args...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	res := &Result{}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 128*1024), 8*1024*1024)
	for sc.Scan() {
		if err := parseBackupLine(sc.Bytes(), res); err != nil {
			return nil, err
		}
	}
	if err := sc.Err(); err != nil {
		_ = cmd.Wait()
		return nil, err
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("restic backup: %w", err)
	}
	if res.SnapshotID == "" {
		return nil, errors.New("restic backup: no summary produced")
	}
	return res, nil
}

// parseBackupLine decodes a single JSON line of restic backup progress and
// stores the final summary into res.
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
		return nil // ignore non-JSON noise (e.g. warnings)
	}
	if msg.MessageType != "summary" && msg.MessageType != "backup.done" {
		return nil
	}
	if msg.Status != "" && msg.Status != "ok" {
		return fmt.Errorf("restic backup reported status %q", msg.Status)
	}
	if msg.SnapshotID == "" {
		return nil // late progress line without its own id; wait for final summary
	}
	res.SnapshotID = msg.SnapshotID
	res.FilesAdded = msg.FilesNew
	res.FilesChanged = msg.FilesChanged
	// restic >=0.17 reports data_added; older versions report bytes_added.
	if msg.BytesAdded != 0 {
		res.BytesAdded = msg.BytesAdded
	} else {
		res.BytesAdded = msg.DataAdded
	}
	res.TotalDuration = time.Duration(msg.TotalDuration * float64(time.Second))
	return nil
}

// LatestSnapshot returns the most recent snapshot, nil if none exist.
func (r *Restic) LatestSnapshot(ctx context.Context) (*Snapshot, error) {
	args := append(r.global(), "--json", "snapshots", "--latest", "1")
	out, err := runCaptureOutput(ctx, r.path, args...)
	if err != nil {
		return nil, fmt.Errorf("restic snapshots: %w", err)
	}
	var snaps []Snapshot
	if err := json.Unmarshal(out, &snaps); err != nil {
		return nil, fmt.Errorf("restic snapshots: invalid JSON: %w", err)
	}
	if len(snaps) == 0 {
		return nil, nil
	}
	return &snaps[0], nil
}

// Restore restores snapshot (or "latest") into target dir, optionally filtered
// by include patterns.
func (r *Restic) Restore(ctx context.Context, snapshot, target string, include []string) error {
	args := append(r.global(), "restore", snapshot, "--target", target)
	for _, in := range include {
		args = append(args, "--include", in)
	}
	if err := runCapture(ctx, r.path, args...); err != nil {
		return fmt.Errorf("restic restore: %w", err)
	}
	return nil
}

// runCapture runs cmd and streams stderr to the agent log while returning
// whether it succeeded.
func runCapture(ctx context.Context, binary string, args ...string) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = os.Stderr // restic CLI status goes to stderr; keep it in the log
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runCaptureOutput runs cmd and returns combined output (used by ping and
// snapshots parsing, where stderr noise is acceptable).
func runCaptureOutput(ctx context.Context, binary string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return []byte(buf.String()), err
}

// normalizedPaths strips trailing separators and backslashes so restic reports
// stable path labels (C:\SoftafriqueBackup vs C:\SoftafriqueBackup\).
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
		// On windows drive roots keep the trailing slash (C:\ has no trim).
		if len(clean) == 2 && clean[1] == ':' && (runtime.GOOS == "windows") {
			clean += string(filepath.Separator)
		}
		out = append(out, clean)
	}
	return out
}