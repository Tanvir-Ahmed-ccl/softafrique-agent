package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"softafrique-backup-agent/internal/api"
	"softafrique-backup-agent/internal/secret"

	"gopkg.in/yaml.v3"
)

// Config holds all agent settings loaded from the YAML config file.
//
// SchemaVersion is the config schema this build understands.
const SchemaVersion = 2

// There are no credentials in this file. The repository password and the device
// password live in the DPAPI-sealed blob managed by package secret, and the
// device id comes from enrollment rather than from this file or the machine
// name.
type Config struct {
	// Version is the config schema version. A config written by a newer
	// installer is refused rather than half-understood.
	Version int `yaml:"version"`

	// Server is the gateway API base URL, e.g.
	// https://backup.softafrique.net/api/v1
	Server string `yaml:"server"`

	// Repo is the restic repository URL, used only when the device is NOT
	// enrolled: for local development and for break-glass recovery. An enrolled
	// device always uses the repository the gateway returned at enrollment, so
	// a stale value here cannot point a paying customer at the wrong place.
	//
	// The literal token {device_id} is replaced at load time. The server serves
	// repositories at the root, not under /repos/:
	//   rest:https://backup.softafrique.net/{device_id}
	Repo string `yaml:"repo"`

	// PasswordFile is a restic --password-file, used only when the device is not
	// enrolled. An enrolled device passes the password to restic in the child
	// process environment instead, so the secret never reaches a command line
	// or a file. Do not set both.
	PasswordFile string `yaml:"password_file"`

	// DeviceID identifies the device when it is not enrolled. For an enrolled
	// device this is informational only: the enrollment-assigned id always
	// wins, because the gateway attributes snapshots by that value.
	DeviceID string `yaml:"device_id"`

	// ResticPath is the path to the restic binary. Defaults to the copy bundled
	// next to the agent exe, then to "restic" on PATH.
	ResticPath string `yaml:"restic_path"`

	// Include is the list of folders to back up. Leave it empty to take the
	// folder the gateway reports in /config (or /enroll); set it to override
	// that, which is what support does for a customer who needs a different
	// folder without a gateway change.
	Include []string `yaml:"include"`

	// Exclude is an optional list of paths/globs to exclude from backups.
	Exclude []string `yaml:"exclude"`

	// ScheduleInterval is how often a backup runs. The gateway's
	// schedule_interval overrides it on an enrolled device.
	ScheduleInterval time.Duration `yaml:"schedule_interval"`

	// RetryInterval is the delay before retrying a failed run. The gateway's
	// retry_interval overrides it on an enrolled device.
	RetryInterval time.Duration `yaml:"retry_interval"`

	// MaxRetries caps retries per scheduled run. -1 (the default) means retry
	// until a run succeeds, which is what a machine that was switched off wants.
	MaxRetries int `yaml:"max_retries"`

	// Jitter adds a random delay of up to this much before each run so that a
	// fleet of agents does not hit the gateway at the same instant. Applied to
	// startup and to retries: a gateway outage otherwise re-synchronises every
	// agent that recovers from it.
	Jitter time.Duration `yaml:"jitter"`

	// ConfigPollInterval is how often /config is refreshed while idle. Zero
	// means derive it from the schedule interval.
	ConfigPollInterval time.Duration `yaml:"config_poll_interval"`

	// StatusHeartbeat is how often an idle agent reports to the gateway so a
	// healthy-but-quiet device is still visible as online.
	StatusHeartbeat time.Duration `yaml:"status_heartbeat"`

	// DataDir stores per-device state: the status file, the log and restic's
	// cache. The credentials blob is deliberately not here; it lives in a
	// machine-wide, separately permissioned store so the service can read it as
	// SYSTEM. "agent doctor -v" prints both paths.
	DataDir string `yaml:"data_dir"`

	// StatusFile receives the machine-readable JSON status for monitoring.
	StatusFile string `yaml:"status_file"`

	// LogFile receives agent logs. Empty means log to stderr only.
	LogFile string `yaml:"log_file"`

	// StatsEnabled turns on periodic `restic stats` readings for the monitoring
	// file. Defaults to true. It is a round trip to the gateway, so it is
	// throttled rather than run after every backup.
	StatsEnabled *bool `yaml:"stats_enabled"`

	// AllowUNC permits a network share (\\server\share\folder) as the folder to
	// protect. Off by default.
	//
	// The reason is the same at run time as at install time: the service runs as
	// LocalSystem, so it reaches a share as the machine account. A path the
	// technician could open while logged in can fail every hourly backup because
	// the machine account has no access, and nothing in status.json would say
	// why, because restic's error and "the share is not reachable" look the same
	// from a dashboard. install.ps1 -AllowUNC writes this key, so a deployment
	// that was allowed once stays allowed after a reinstall.
	//
	// This is the only escape hatch in the protected-folder policy. Optical and
	// removable drives have none, because an ejected disc is not a
	// misconfiguration to be tolerated but a backup that silently stops existing.
	AllowUNC bool `yaml:"allow_unc"`

	// Verbose enables debug-level logging.
	Verbose bool `yaml:"verbose"`
}

// Default returns a Config populated with platform-appropriate defaults.
func Default() *Config {
	dataDir := DefaultDataDir()
	return &Config{
		Server:             api.DefaultBaseURL,
		ResticPath:         "restic",
		ScheduleInterval:   1 * time.Hour,
		RetryInterval:      5 * time.Minute,
		MaxRetries:         -1,
		Jitter:             30 * time.Second,
		ConfigPollInterval: 15 * time.Minute,
		StatusHeartbeat:    6 * time.Hour,
		DataDir:            dataDir,
		StatusFile:         filepath.Join(dataDir, "status.json"),
		LogFile:            filepath.Join(dataDir, "agent.log"),
		PasswordFile:       filepath.Join(dataDir, "repo.password"),
		// Repositories are served at the server root: /{device_id}/.
		Repo: "rest:https://backup.softafrique.net/{device_id}",
	}
}

// DataDirACLWarning returns a warning when the data directory is not the
// canonical per-machine location.
//
// On Windows the credentials blob is protected by DPAPI in machine scope, which
// anything running as SYSTEM or an administrator can decrypt. The thing that
// actually limits that is the directory ACL, and the ACL is set by the MSI. An
// agent pointed at a hand-made directory has no such protection, so say so
// loudly rather than pretending the blob is sealed.
func (c *Config) DataDirACLWarning() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	want := filepath.Clean(DefaultDataDir())
	got := filepath.Clean(c.DataDir)
	if strings.EqualFold(want, got) {
		return ""
	}
	return "data_dir " + c.DataDir + " is not the standard " + want +
		"; its access control list is whatever it was created with, so the credentials blob there is only protected against non-administrators. " +
		"Reinstall from the MSI, or have an administrator restrict the folder to SYSTEM and Administrators."
}

// Stats reports whether periodic repository usage readings are enabled.
func (c *Config) Stats() bool {
	if c.StatsEnabled == nil {
		return true
	}
	return *c.StatsEnabled
}

// BackupPaths returns the folders to back up, preferring an explicit include
// over serverPath. Empty when neither is set, which the caller must treat as an
// error rather than defaulting silently.
func (c *Config) BackupPaths(serverPath string) []string {
	if len(c.Include) > 0 {
		return c.Include
	}
	if p := strings.TrimSpace(serverPath); p != "" {
		return []string{p}
	}
	return nil
}

// SecretStore is the credentials blob path inside the data directory.
func (c *Config) SecretStore() *secret.Store {
	return secret.NewStore(filepath.Join(c.DataDir, secret.FileName))
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

// DefaultBackupSource returns the default folder to back up when nothing else
// says otherwise.
func DefaultBackupSource() string {
	if runtime.GOOS == "windows" {
		return filepath.Join("C:", "SoftafriqueBackup")
	}
	return filepath.Join(".", "SoftafriqueBackup")
}

// Load reads, parses and validates the config at path, applying overrides.
func Load(path string, overrides map[string]string) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := loadYAML(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	applyOverrides(cfg, overrides)
	applyDefaults(cfg)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// legacyKeys are keys that used to be part of the schema and are now ignored
// rather than rejected.
//
// Rejecting them would be the stricter choice and the wrong one. The MSI writes
// config.yaml with NeverOverwrite, so a device upgrading in place keeps the file
// it was given, including keys that build happened to ship. Refusing to start
// because of a key the agent no longer acts on would turn "this option is gone"
// into "this device is unprotected", which is the worst possible trade for a
// backup agent.
//
// Each entry exists because a released build could write it. Remove one only
// once no supported upgrade path can carry it.
//
//	auto_init: the gateway creates the repository at enrollment and fails the
//	          enrollment if its restic init does not, so the agent no longer has
//	          any init code path. A stale key here is not an error, it is a
//	          no-op, and the agent cannot act on it either way.
var legacyKeys = map[string]string{
	"auto_init": "the gateway creates the repository at enrollment; the agent has no init code path",
}

// loadYAML decodes into cfg, rejecting keys the schema does not have.
func loadYAML(data []byte, cfg *Config) error {
	// Unknown keys are an error, not a shrug.
	//
	// This file decides where the encryption key is protected and where the log
	// is written. A typo such as "state-dir" instead of "data_dir" used to be
	// ignored in silence, and the agent then wrote credentials to a default
	// location nobody had applied the intended access control to. A loud failure
	// at startup is the only safe response to a key nobody read.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		if isOnlyLegacyKeys(err) {
			// Retry without them. A config carrying a retired key is a config
			// that should work, and the error it produced is about our own
			// bookkeeping rather than about anything the operator wrote.
			cleaned, err := withoutLegacyKeys(data)
			if err != nil {
				return err
			}
			dec = yaml.NewDecoder(bytes.NewReader(cleaned))
			dec.KnownFields(true)
			if err := dec.Decode(cfg); err != nil {
				if !errors.Is(err, io.EOF) {
					return err
				}
			}
			return nil
		}
		// An empty file decodes to io.EOF, which is a valid empty config that
		// applyDefaults and Validate then finish off.
		if !errors.Is(err, io.EOF) {
			return err
		}
	}
	return nil
}

// isOnlyLegacyKeys reports whether err is nothing more than unknown-field errors
// for keys that used to exist.
//
// A config with a retired key *and* a genuine typo must still fail. The check
// is deliberately strict about that: every offending line in the error has to
// name a legacy key, or the operator's typo goes unnoticed because something
// unrelated was also wrong.
func isOnlyLegacyKeys(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !strings.Contains(msg, "field ") || !strings.Contains(msg, "not found") {
		return false
	}
	// yaml.v3 formats these as:
	//   yaml: unmarshal errors:
	//     line 42: field auto_init not found in type config.Config
	const marker = "field "
	rest := msg
	found := 0
	for {
		i := strings.Index(rest, marker)
		if i < 0 {
			break
		}
		rest = rest[i+len(marker):]
		sp := strings.IndexAny(rest, " \n")
		if sp < 0 {
			return false
		}
		name := rest[:sp]
		if _, ok := legacyKeys[name]; !ok {
			return false
		}
		found++
		rest = rest[sp:]
	}
	return found > 0
}

// withoutLegacyKeys returns data with the retired keys removed.
func withoutLegacyKeys(data []byte) ([]byte, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	for k := range legacyKeys {
		delete(doc, k)
	}
	return yaml.Marshal(doc)
}

func applyDefaults(cfg *Config) {
	if cfg.ResticPath == "" || cfg.ResticPath == "restic" {
		if p := bundledRestic(); p != "" {
			cfg.ResticPath = p
		} else {
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
	if cfg.Jitter < 0 {
		cfg.Jitter = 0
	}
	if cfg.ConfigPollInterval <= 0 {
		cfg.ConfigPollInterval = 15 * time.Minute
	}
	if cfg.StatusHeartbeat <= 0 {
		cfg.StatusHeartbeat = 6 * time.Hour
	}
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
	if cfg.Server == "" {
		cfg.Server = api.DefaultBaseURL
	}
	// Canonicalise once here so everything downstream can assume an https URL
	// with no trailing slash. Validate reports an unparseable value.
	if normalized, err := api.NormalizeBaseURL(cfg.Server); err == nil {
		cfg.Server = normalized
	}
	cfg.Repo = strings.ReplaceAll(cfg.Repo, "{device_id}", cfg.DeviceID)
}

// bundledRestic returns the path to a restic binary bundled next to the agent
// exe, if any, so the MSI can ship restic beside the agent with no config.
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

// Validate ensures the config is structurally usable.
//
// It deliberately does not check that the device can actually back up: that
// depends on whether the credentials blob exists, which the agent knows and
// this package does not. The agent refuses to run rather than failing later
// inside restic.
func (c *Config) Validate() error {
	// The version key was in the example config and the installer template from
	// the start, and nothing ever read it. A config written by a newer installer
	// and read by an older agent is exactly the case a version key exists for, so
	// it is honoured now rather than decorative.
	if c.Version != 0 && c.Version != SchemaVersion {
		return fmt.Errorf("config: version %d is not supported by this agent, which understands version %d; "+
			"upgrade the agent or restore the matching config", c.Version, SchemaVersion)
	}
	if _, err := api.NormalizeBaseURL(c.Server); err != nil {
		return err
	}
	if c.Repo == "" {
		return errors.New("config: repo must not be empty")
	}
	if c.DataDir == "" {
		return errors.New("config: data_dir must not be empty")
	}
	// A 0.1.0 config carries the device password inside the repository URL,
	// because that was the only place restic would take it from by hand. Catch
	// it at load time rather than putting it back in a command line.
	if strings.Contains(c.Repo, "@") {
		return errors.New("config: repo must not embed credentials; the device password is delivered in restic's " +
			"environment instead, and the gateway returns a credential-free repository at enrollment")
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
		cfg.Include = splitList(v)
	}
	if v, ok := overrides["device-id"]; ok {
		cfg.DeviceID = v
	}
	if v, ok := overrides["server"]; ok {
		cfg.Server = v
	}
}

func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
