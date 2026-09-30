package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"softafrique-backup-agent/internal/config"
	"softafrique-backup-agent/internal/pathpolicy"
	"softafrique-backup-agent/internal/restic"
	"softafrique-backup-agent/internal/secret"
)

// probeRestic builds a restic wrapper for credentials that have not been stored
// yet, so enrollment can test a repository before committing to it.
func (a *Agent) probeRestic(creds *secret.Credentials) *restic.Restic {
	return restic.New(restic.Options{
		Binary:     a.cfg.ResticPath,
		Repository: creds.Repository,
		Host:       creds.DeviceID,
		Password:   creds.ResticPassword(),
		// Enrollment never creates a repository and neither does anything else.
		// There is no option to turn init on; see restic.EnsureRepo.
		Logger: a.log,
	})
}

// enrollBackupPath decides which folder to ask the gateway to protect.
//
// An explicit include in config.yaml is the operator's deliberate choice and
// wins. Otherwise the platform default, which is where customers keep their
// business data, is proposed to the dashboard for confirmation at enrollment.
func (a *Agent) enrollBackupPath() string {
	if len(a.cfg.Include) > 0 {
		return a.cfg.Include[0]
	}
	return config.DefaultBackupSource()
}

// insideDir reports whether path is strictly inside dir, so a migration can
// delete a file the agent created without ever touching one an operator put
// somewhere else on purpose.
func insideDir(dir, path string) bool {
	dir = strings.ToLower(filepath.Clean(dir))
	path = strings.ToLower(filepath.Clean(path))
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// fileExists reports whether path is a readable regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// ensureDir creates dir if it is missing.
func ensureDir(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

// ensureBackupPaths makes sure every folder the agent was asked to protect
// exists, and returns the ones it had to create.
//
// A path that exists but is a *file* is left alone and not reported as created:
// restic will fail on it with a clear error, which is a better outcome than the
// agent deleting a customer's file to make room for a directory.
//
// Failure to create is not fatal here either. restic's own error for an
// uncreatable path names the path and the reason, and inventing a second,
// vaguer error at this point would only get in the way of the real one.
func (a *Agent) ensureBackupPaths(paths []string) []string {
	var created []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		info, err := os.Stat(p)
		if err == nil {
			if info.IsDir() {
				continue
			}
			a.log.Warn("the protected path is a file, not a folder", "path", p)
			continue
		}
		if !os.IsNotExist(err) {
			a.log.Warn("cannot read the protected path", "path", p, "err", err)
			continue
		}
		if mkErr := os.MkdirAll(p, 0o755); mkErr != nil {
			a.log.Warn("could not recreate the protected folder; the backup will report the failure",
				"path", p, "err", mkErr)
			continue
		}
		a.log.Warn("the protected folder was missing and has been recreated; the next backup will "+
			"capture an empty folder until the customer restores their data",
			"path", p)
		created = append(created, p)
	}
	return created
}

// refusedPaths returns the paths the agent will not protect, with the reason for
// each.
//
// This is the run-time half of a rule the installer already applies, and it is
// not redundant. The install-time check catches what a technician typed; this
// catches what the gateway later says, in a backup_path on /config or in an
// enrollment reply -- and a share or a DVD drive letter is exactly the kind of
// thing a dashboard can be made to say by mistake, by an import, or by a support
// engineer guessing. The consequences of getting it wrong are the agent running
// as LocalSystem and authenticating to a remote machine on a schedule, or
// reporting a green backup of an empty disc tray. Neither leaves a trace in a
// backup report.
//
// The rule itself lives in internal/pathpolicy, which the MSI's validatepath
// applies to the typed path at install time. Sharing it is the point: the two
// checks happen a year apart, and a second copy of this decision is a decision
// that eventually disagrees with itself.
//
// The refusal is quiet about the folder and loud about the reason: the path is
// left strictly alone, and the attempt is abandoned rather than handed to restic.
// An operator who means a share sets allow_unc and is explicit about having done
// so. Nothing here has an opt-in for an optical or removable drive, because an
// ejected disc is not a configuration to be tolerated.
func (a *Agent) refusedPaths(paths []string) []*pathpolicy.Error {
	var refused []*pathpolicy.Error
	for _, p := range paths {
		if p == "" {
			continue
		}
		// The opt-in is named the way this process can act on it. The installer's
		// copy of the same refusal says "-allow-unc" because that is what a
		// technician types; this one says the config key, because at three in
		// the morning the only thing anybody can do is edit a file.
		if err := pathpolicy.RequireFixed(p, a.cfg.AllowUNC, "allow_unc: true in config.yaml"); err != nil {
			var perr *pathpolicy.Error
			if errors.As(err, &perr) {
				refused = append(refused, perr)
				continue
			}
			// RequireFixed only ever returns *Error, but an unforeseen error type
			// must not be read as permission to carry on backing up.
			refused = append(refused, &pathpolicy.Error{Path: p, Kind: pathpolicy.KindUnknown})
		}
	}
	return refused
}

// refusalError builds the sentence that goes into status.json.
//
// It is one sentence naming the paths, because that is the line a support
// engineer reads in a status file at three in the morning, and a message that
// said only "policy violation" would tell them nothing they could act on.
func refusalError(refused []*pathpolicy.Error) error {
	paths := make([]string, 0, len(refused))
	shares := false
	for _, r := range refused {
		paths = append(paths, fmt.Sprintf("%s (on %s)", r.Path, r.Kind))
		if r.Kind == pathpolicy.KindNetwork {
			shares = true
		}
	}
	msg := fmt.Sprintf("refusing to protect %s: the folder to protect must be on a fixed disk",
		strings.Join(paths, ", "))
	if shares {
		// Only mentioned when a share is actually involved: the LocalSystem
		// explanation is true and important, but appending it to a refused DVD
		// drive trains a reader to skip the reason.
		msg += ". A network share is refused because the agent runs as LocalSystem and would reach it" +
			" as the machine account, not as the technician who set it up, so a share that opened during" +
			" installation can still fail every hourly backup with nothing to show for it. If that is" +
			" deliberate, set allow_unc: true in config.yaml or reinstall with install.ps1 -AllowUNC"
	}
	return errors.New(msg)
}
