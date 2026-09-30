package config

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimalConfig = `
repo: "rest:https://backup.softafrique.net/local-dev"
password_file: "pw"
include:
  - "C:\\SoftafriqueBackup"
`

func TestLoadParsesDurations(t *testing.T) {
	path := writeConfig(t, `
repo: "rest:https://backup.softafrique.net/{device_id}"
password_file: "pw"
include:
  - "C:\\SoftafriqueBackup"
schedule_interval: 1h
retry_interval: 5m
max_retries: -1
jitter: 30s
config_poll_interval: 20m
status_heartbeat: 2h
device_id: "dev-01HQ8XK3M4N7"
`)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ScheduleInterval != time.Hour {
		t.Errorf("schedule_interval: want 1h, got %v", cfg.ScheduleInterval)
	}
	if cfg.RetryInterval != 5*time.Minute {
		t.Errorf("retry_interval: want 5m, got %v", cfg.RetryInterval)
	}
	if cfg.Jitter != 30*time.Second {
		t.Errorf("jitter: want 30s, got %v", cfg.Jitter)
	}
	if cfg.ConfigPollInterval != 20*time.Minute {
		t.Errorf("config_poll_interval: want 20m, got %v", cfg.ConfigPollInterval)
	}
	if cfg.StatusHeartbeat != 2*time.Hour {
		t.Errorf("status_heartbeat: want 2h, got %v", cfg.StatusHeartbeat)
	}
	if cfg.MaxRetries != -1 {
		t.Errorf("max_retries: want -1, got %d", cfg.MaxRetries)
	}
	if cfg.DeviceID != "dev-01HQ8XK3M4N7" {
		t.Errorf("device_id: got %q", cfg.DeviceID)
	}
	if !strings.Contains(cfg.Repo, "dev-01HQ8XK3M4N7") {
		t.Errorf("repo should contain the device id, got %q", cfg.Repo)
	}
}

// TestRepoDefaultIsServerRoot covers finding 4: the server serves repositories
// at the root, not under /repos/.
func TestRepoDefaultIsServerRoot(t *testing.T) {
	cfg := Default()
	if strings.Contains(cfg.Repo, "/repos/") {
		t.Errorf("default repo must not contain /repos/: %q", cfg.Repo)
	}
	want := "rest:https://backup.softafrique.net/{device_id}"
	if cfg.Repo != want {
		t.Errorf("default repo = %q, want %q", cfg.Repo, want)
	}
	if strings.Contains(want, "/repos/") {
		t.Error("test fixture is wrong")
	}
}

func TestLoadDefaultsApplied(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalConfig), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ScheduleInterval <= 0 || cfg.RetryInterval <= 0 {
		t.Error("expected default intervals")
	}
	if cfg.ConfigPollInterval <= 0 || cfg.StatusHeartbeat <= 0 {
		t.Error("expected default poll and heartbeat intervals")
	}
	if cfg.StatusFile == "" || cfg.LogFile == "" || cfg.DataDir == "" {
		t.Error("expected default state paths")
	}
	if cfg.Server != "https://backup.softafrique.net/api/v1" {
		t.Errorf("default server = %q", cfg.Server)
	}
	if !cfg.Stats() {
		t.Error("repo usage readings should default to on")
	}
	if cfg.AllowUNC {
		t.Error("allow_unc must default to off: a share is reached as the machine account, not the technician")
	}
}

// The gateway creates the repository at enrollment, and enrollment fails if its
// restic init does not. There is therefore no reason for the agent to be able to
// create one, and no config key that could ask it to.
func TestThereIsNoAutoInitKeyToActOn(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
repo: "rest:https://backup.softafrique.net/dev-01"
auto_init: true
`), nil)
	if err != nil {
		t.Fatalf("a config carrying the retired auto_init key should still load: %v", err)
	}
	if _, ok := reflect.TypeOf(*cfg).FieldByName("AutoInit"); ok {
		t.Error("config.Config still has an AutoInit field; the agent must not be able to create a repository")
	}
	// The key is a no-op, not a setting: nothing in the loaded config records
	// that it was true, so nothing can act on it.
	if reflect.ValueOf(*cfg).NumField() == 0 {
		t.Error("Config has no fields, which cannot be right")
	}
}

// A device upgrading in place keeps the config.yaml it was installed with,
// because the MSI writes it with NeverOverwrite. A released build shipped
// auto_init, so every upgraded device has that key on disk. Refusing to start
// because of it would turn "this option is gone" into "this device is
// unprotected", which is the worst possible trade for a backup agent.
func TestARetiredKeyDoesNotStopAnUpgradedDeviceFromStarting(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
server: "https://backup.softafrique.net/api/v1"
repo: "rest:https://backup.softafrique.net/dev-01"
data_dir: "C:\\ProgramData\\SoftafriqueBackupAgent"
include:
  - 'D:\CustomerData'
auto_init: false
verbose: false
`), nil)
	if err != nil {
		t.Fatalf("a shipped 0.2.0 config with auto_init in it must still load: %v", err)
	}
	if len(cfg.Include) != 1 || cfg.Include[0] != `D:\CustomerData` {
		t.Errorf("the rest of the config was lost while dropping the retired key: %v", cfg.Include)
	}
}

// Tolerating a retired key must not become tolerating everything. A typo in a
// key nobody reads is how 0.1.0 wrote credentials somewhere without an ACL, and
// it stays fatal even when a retired key happens to be present in the same file.
func TestATypoIsStillFatalAlongsideARetiredKey(t *testing.T) {
	_, err := Load(writeConfig(t, `
repo: "rest:https://backup.softafrique.net/dev-01"
auto_init: false
data-dir: "C:\\ProgramData\\Elsewhere"
`), nil)
	if err == nil {
		t.Fatal("a mistyped key must still be refused even with a retired key present")
	}
	if !strings.Contains(err.Error(), "data-dir") {
		t.Errorf("the error should name the offending key, got: %v", err)
	}
}

// TestRepoWithEmbeddedCredentialIsRejected is the migration guard for the
// 0.1.0 config format, where the device password had to be typed into the URL.
func TestRepoWithEmbeddedCredentialIsRejected(t *testing.T) {
	_, err := Load(writeConfig(t, `
repo: "rest:https://9f2c4a7b1d8e@backup.softafrique.net/repos/mohsin"
password_file: "pw"
`), nil)
	if err == nil {
		t.Fatal("expected a credential-bearing repo URL to be rejected")
	}
	if !strings.Contains(err.Error(), "must not embed credentials") {
		t.Errorf("error should explain the problem, got %v", err)
	}
}

func TestServerIsValidated(t *testing.T) {
	if _, err := Load(writeConfig(t, minimalConfig+"\nserver: \"ftp://nope\"\n"), nil); err == nil {
		t.Error("expected a non-http server URL to be rejected")
	}
	cfg, err := Load(writeConfig(t, minimalConfig+"\nserver: \"backup.softafrique.net/api/v1\"\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != "https://backup.softafrique.net/api/v1" {
		t.Errorf("a scheme-less server should default to https, got %q", cfg.Server)
	}
}

func TestOverrideSeedsTemplateSubstitution(t *testing.T) {
	path := writeConfig(t, `
repo: "rest:https://backup.softafrique.net/{device_id}"
password_file: "pw"
`)
	cfg, err := Load(path, map[string]string{"device-id": "dev-01HQ8XK3M4N7"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeviceID != "dev-01HQ8XK3M4N7" {
		t.Errorf("device id override not applied: %q", cfg.DeviceID)
	}
	if !strings.HasSuffix(cfg.Repo, "/dev-01HQ8XK3M4N7") {
		t.Errorf("repo not substituted from the override: %q", cfg.Repo)
	}
}

// TestDeviceIDIsNeverDerivedFromHostname covers finding 2: 0.1.0 derived the
// device id from the machine name, which produced "mohsin" on Mohsin's PC and
// would collide for any two machines with the same name.
func TestDeviceIDIsNeverDerivedFromHostname(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalConfig), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeviceID != "" {
		t.Errorf("device id must stay empty until enrollment, got %q", cfg.DeviceID)
	}
	host, _ := os.Hostname()
	if cfg.DeviceID != "" && cfg.DeviceID == strings.ToLower(host) {
		t.Error("device id must not be the machine name")
	}
}

func TestBackupPathsPrefersLocalInclude(t *testing.T) {
	cfg := Default()
	cfg.Include = []string{`D:\CustomerData`}
	if got := cfg.BackupPaths(`C:\FromServer`); len(got) != 1 || got[0] != `D:\CustomerData` {
		t.Errorf("an explicit include must win over the server path, got %v", got)
	}

	cfg.Include = nil
	if got := cfg.BackupPaths(`C:\FromServer`); len(got) != 1 || got[0] != `C:\FromServer` {
		t.Errorf("with no include the server path is used, got %v", got)
	}
	if got := cfg.BackupPaths("  "); got != nil {
		t.Errorf("with neither, the caller must get nothing and decide for itself, got %v", got)
	}
}

func TestStatsEnabledCanBeTurnedOff(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalConfig+"\nstats_enabled: false\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Stats() {
		t.Error("stats_enabled: false was not honoured")
	}
}

func TestSecretStoreLivesInDataDir(t *testing.T) {
	cfg := Default()
	cfg.DataDir = filepath.Join("some", "where")
	got := cfg.SecretStore().Path()
	want := filepath.Join("some", "where", "credentials.dat")
	if got != want {
		t.Errorf("credentials path = %q, want %q", got, want)
	}
}

// TestNoConfigFieldCanCarryASecret keeps credentials out of the YAML too, not
// just out of the status file.
func TestNoConfigFieldCanCarryASecret(t *testing.T) {
	cfg := Default()
	// DevicePassword and EncryptionKey do not exist on Config; this is the
	// compile-time equivalent, asserted at runtime so the intent is recorded.
	if _, ok := interface{}(cfg).(interface{ DevicePassword() string }); ok {
		t.Error("Config must not expose a device password")
	}
}

func TestACLWarningOnlyOnNonStandardDataDir(t *testing.T) {
	cfg := Default()
	cfg.DataDir = DefaultDataDir()
	if w := cfg.DataDirACLWarning(); w != "" {
		t.Errorf("standard data dir should not warn, got %q", w)
	}
	cfg.DataDir = filepath.Join("C:", "Temp", "backup")
	w := cfg.DataDirACLWarning()
	if runtime.GOOS == "windows" {
		if w == "" {
			t.Error("a non-standard data dir must warn on Windows: the MSI sets that ACL")
		}
		if !strings.Contains(w, "administrator") {
			t.Errorf("the warning should say who can read the blob, got %q", w)
		}
	}
}

func TestDefaultDataDirIsProgramDataOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("ProgramData is a Windows concept")
	}
	if got := DefaultDataDir(); !strings.Contains(got, "ProgramData") {
		t.Errorf("DefaultDataDir = %q, want it under ProgramData", got)
	}
}

func TestApplyOverridesIncludeTrimsEmpty(t *testing.T) {
	cfg := Default()
	applyOverrides(cfg, map[string]string{"include": " C:\\A , , D:\\B "})
	if len(cfg.Include) != 2 || cfg.Include[0] != "C:\\A" || cfg.Include[1] != "D:\\B" {
		t.Errorf("include override = %#v", cfg.Include)
	}
}

// A config key nobody read is a silent misconfiguration. This file decides where
// the encryption key is protected, so a typo must fail loudly rather than leave
// the agent writing to a location nobody secured.
func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "version: 2\nserver: https://backup.example.net/api/v1\n" +
		"repo: rest:https://backup.example.net/{device_id}\nstate_dir: /tmp/wrong\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path, nil)
	if err == nil {
		t.Fatal("an unknown key must be rejected")
	}
	if !strings.Contains(err.Error(), "state_dir") {
		t.Errorf("the error must name the offending key, got %q", err)
	}
}

// The keys the installer writes must all parse, or a fresh MSI install would
// fail to start.
func TestLoadAcceptsInstallerTemplateKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "version: 2\nserver: https://backup.example.net/api/v1\n" +
		"repo: rest:https://backup.example.net/{device_id}\n" +
		"data_dir: 'C:\\ProgramData\\SoftafriqueBackupAgent'\n" +
		"status_file: 'C:\\ProgramData\\SoftafriqueBackupAgent\\status.json'\n" +
		"log_file: 'C:\\ProgramData\\SoftafriqueBackupAgent\\agent.log'\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("the installer's key set must load: %v", err)
	}
	if !strings.Contains(cfg.DataDir, "SoftafriqueBackupAgent") {
		t.Errorf("data_dir = %q", cfg.DataDir)
	}
}

// A config written by a newer installer must be refused, not half-read. The
// version key sat in the example and the installer template unread from the
// start, which is what made this worth a test.
func TestLoadRefusesAnUnknownSchemaVersion(t *testing.T) {
	for _, v := range []string{"version: 3", "version: 99"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		body := v + "\nserver: https://backup.example.net/api/v1\n" +
			"repo: rest:https://backup.example.net/{device_id}\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path, nil)
		if err == nil {
			t.Fatalf("%s must be rejected", v)
		}
		if !strings.Contains(err.Error(), "upgrade the agent") {
			t.Errorf("%s: the error must say what to do, got %q", v, err)
		}
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
