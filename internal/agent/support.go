package agent

import (
	"os"
	"path/filepath"
	"strings"

	"softafrique-backup-agent/internal/config"
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
		// Enrollment must never create a repository: the gateway owns that. If
		// the repository is missing here, the operator needs to know, not have
		// the agent quietly make an empty one somewhere unexpected.
		AllowInit: false,
		Logger:    a.log,
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
