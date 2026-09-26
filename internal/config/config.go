package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds all agent settings loaded from the YAML config file.
type Config struct {
	// Repo is the restic repository URL. The literal token {device_id} is
	// replaced with the agent's device ID at load time. Example:
	//   rest:https://backup.softafrique.net/repos/{device_id}
	Repo string `yaml:"repo"`

	// PasswordFile points to the file containing the restic repository
	// password (the encryption passphrase). It must exist on first run.
	PasswordFile string `yaml:"password_file"`

	// ResticPath is the path to the restic binary. Defaults to "restic"
	// (found on PATH) unless bundled next to the agent.
	ResticPath string `yaml:"restic_path"`

	// Include is the list of folders to back up. Example: C:\SoftafriqueBackup
	Include []string `yaml:"include"`

	// Exclude is an optional list of paths/globs to exclude from backups.
	Exclude []string `yaml:"exclude"`

	// ScheduleInterval is how often a backup should run (e.g. 1h, 30m).
	ScheduleInterval time.Duration `yaml:"schedule_interval"`

	// RetryInterval is the delay between retry attempts when a backup fails
	// (e.g. the PC was offline). Defaults to 5m.
	RetryInterval time.Duration `yaml:"retry_interval"`

	// MaxRetries caps the number of retries per scheduled backup. -1 (default)
	// means retry until the next scheduled run succeeds.
	MaxRetries int `yaml:"max_retries"`

	// Jitter adds a random delay of up to this amount before each run so that
	// many customers' agents do not all hit the server at the exact same time.
	Jitter time.Duration `yaml:"jitter"`

	// DataDir stores per-device state (credentials, repo lock, status).
	DataDir string `yaml:"data_dir"`

	// StatusFile receives the machine-readable JSON status for monitoring.
	StatusFile string `yaml:"status_file"`

	// LogFile receives agent logs. Empty means log to stderr only.
	LogFile string `yaml:"log_file"`

	// DeviceID is an optional stable customer/device identifier. When empty it
	// is derived from the hostname.
	DeviceID string `yaml:"device_id"`

	// Verbose enables debug-level logging.
	Verbose bool `yaml:"verbose"`
}

// Default returns a Config populated with platform-appropriate defaults.
func Default() *Config {
	dataDir := DefaultDataDir()
	return &Config{
		ResticPath:        "restic",
		ScheduleInterval:  1 * time.Hour,
		RetryInterval:     5 * time.Minute,
		MaxRetries:        -1,
		Jitter:            30 * time.Second,
		DataDir:           dataDir,
		StatusFile:        filepath.Join(dataDir, "status.json"),
		LogFile:           filepath.Join(dataDir, "agent.log"),
		PasswordFile:      filepath.Join(dataDir, "repo.password"),
		Include:           []string{DefaultBackupSource()},
		Repo:              "rest:https://backup.softafrique.net/repos/{device_id}",
	}
}

// DefaultDataDir returns the agent state directory for the platform.
func DefaultDataDir() string {
	if runtime.GOOS == "windows" {
		base := os.Getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		return filepath.Join(base, "SoftafriqueBackupAgent")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "softafrique-backup-agent")
}

// DefaultConfigPath returns where the agent looks for its config file.
func DefaultConfigPath() string {
	return filepath.Join(DefaultDataDir(), "config.yaml")
}

// DefaultBackupSource returns the default folder to back up.
func DefaultBackupSource() string {
	return filepath.Join("C:", "SoftafriqueBackup")
}

// Load reads, parses and validates the config at path. optsOverride is applied
// on top of the file values.
func Load(path string, overrides map[string]string) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	applyOverrides(cfg, overrides)
	applyDefaults(cfg)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}



func applyDefaults(cfg *Config) {
	if cfg.ResticPath == "" || cfg.ResticPath == "restic" {
		if p := bundledRestic(); p != "" {
			cfg.ResticPath = p
		} else if cfg.ResticPath == "" {
			cfg.ResticPath = "restic"
		}
	}
	if cfg.ScheduleInterval <= 0 {
		cfg.ScheduleInterval = 1 * time.Hour
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = 5 * time.Minute
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = -1
	}
	// Duration fields are int-nanoseconds in the struct; when unmarshalled from
	// YAML we already use time.Duration. Nothing to do here.
	if cfg.DataDir == "" {
		cfg.DataDir = DefaultDataDir()
	}
	if cfg.StatusFile == "" {
		cfg.StatusFile = filepath.Join(cfg.DataDir, "status.json")
	}
	if cfg.LogFile == "" {
		cfg.LogFile = filepath.Join(cfg.DataDir, "agent.log")
	}
	if cfg.PasswordFile == "" {
		cfg.PasswordFile = filepath.Join(cfg.DataDir, "repo.password")
	}
	if len(cfg.Include) == 0 {
		cfg.Include = []string{DefaultBackupSource()}
	}
	if cfg.DeviceID == "" {
		cfg.DeviceID = HostDeviceID()
	}
	cfg.Repo = strings.ReplaceAll(cfg.Repo, "{device_id}", cfg.DeviceID)
}

// bundledRestic returns the path to a restic binary bundled next to the agent
// exe, if any. This lets the Windows MSI ship restic beside the exe with zero
// config. On Windows it looks for restic.exe; elsewhere for `restic`.
func bundledRestic() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	names := []string{"restic"}
	if runtime.GOOS == "windows" {
		names = []string{"restic.exe"}
	}
	for _, name := range names {
		cand := filepath.Join(filepath.Dir(exe), name)
		if info, err := os.Stat(cand); err == nil && !info.IsDir() {
			return cand
		}
	}
	return ""
}

// Validate ensures the config is usable.
func (c *Config) Validate() error {
	if c.Repo == "" {
		return errors.New("config: repo must not be empty")
	}
	if c.PasswordFile == "" {
		return errors.New("config: password_file must not be empty")
	}
	if len(c.Include) == 0 {
		return errors.New("config: at least one include path is required")
	}
	return nil
}

func applyOverrides(cfg *Config, overrides map[string]string) {
	if overrides == nil {
		return
	}
	if v, ok := overrides["repo"]; ok {
		cfg.Repo = v
	}
	if v, ok := overrides["password-file"]; ok {
		cfg.PasswordFile = v
	}
	if v, ok := overrides["restic-path"]; ok {
		cfg.ResticPath = v
	}
	if v, ok := overrides["include"]; ok {
		cfg.Include = strings.Split(v, ",")
	}
	if v, ok := overrides["device-id"]; ok {
		cfg.DeviceID = v
		cfg.Repo = strings.ReplaceAll(cfg.Repo, "{device_id}", v)
	}
}

// HostDeviceID derives a stable device identifier from the hostname.
func HostDeviceID() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "unknown-device"
	}
	name = strings.ToLower(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune('-')
		default:
			b.WriteRune('-')
		}
	}
	id := b.String()
	if len(id) > 63 {
		id = id[:63]
	}
	return id
}