package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostDeviceID(t *testing.T) {
	// Stable, lowercase, alnum+dash only.
	id := HostDeviceID()
	if id == "" {
		t.Fatal("empty device id")
	}
	var prev string
	for i := 0; i < 100; i++ {
		cur := HostDeviceID()
		if prev != "" && cur != prev {
			t.Fatalf("device id unstable: %q != %q", cur, prev)
		}
		prev = cur
	}
}

func TestLoad_ParsesDurations(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `
repo: "rest:https://backup.softafrique.net/repos/{device_id}"
password_file: "pw"
include:
  - "C:\\SoftafriqueBackup"
schedule_interval: 1h
retry_interval: 5m
max_retries: -1
jitter: 30s
device_id: "cust-42"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath, nil)
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
	if cfg.DeviceID != "cust-42" {
		t.Errorf("device_id: want cust-42, got %q", cfg.DeviceID)
	}
	if !strings.Contains(cfg.Repo, "cust-42") {
		t.Errorf("repo should contain device id, got %q", cfg.Repo)
	}
}

func TestLoad_DefaultsApplied(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `
repo: "rest:https://backup.softafrique.net/repos/{device_id}"
password_file: "pw"
include:
  - "C:\\SoftafriqueBackup"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ScheduleInterval <= 0 {
		t.Error("expected default schedule interval")
	}
	if cfg.RetryInterval <= 0 {
		t.Error("expected default retry interval")
	}
	if cfg.StatusFile == "" || cfg.LogFile == "" || cfg.DataDir == "" {
		t.Error("expected default state paths")
	}
	if cfg.DeviceID == "" {
		t.Error("expected derived device id")
	}
	if strings.Contains(cfg.Repo, "{device_id}") {
		t.Error("device_id placeholder should have been substituted")
	}
}

func TestLoad_TemplateSubstitutionSeededFromOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `repo: "rest:https://backup.softafrique.net/repos/{device_id}"
password_file: "pw"
include:
  - "C:\\SoftafriqueBackup"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath, map[string]string{"device-id": "shared"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeviceID != "shared" || !strings.HasSuffix(cfg.Repo, "/shared") {
		t.Errorf("override not applied: device=%q repo=%q", cfg.DeviceID, cfg.Repo)
	}
}