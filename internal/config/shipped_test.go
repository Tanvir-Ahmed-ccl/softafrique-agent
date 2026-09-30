package config

import (
	"strings"
	"testing"
)

// The MSI substitutes a handful of placeholders when it writes config.yaml, and
// the substituted result is what the agent has to parse at startup. Parsing the
// raw template proves less than it looks like it proves: a placeholder that is
// not valid YAML until it is filled in passes the raw test and then stops a
// fresh install dead. So the template is tested the way it is installed, with
// the values a real deployment would substitute.
var templateValues = map[string]string{
	"[DATADIR]":    `C:\ProgramData\SoftafriqueBackupAgent`,
	"[STATUSFILE]": `C:\ProgramData\SoftafriqueBackupAgent\status.json`,
	"[LOGFILE]":    `C:\ProgramData\SoftafriqueBackupAgent\agent.log`,
	"[BACKUPPATH]": `C:\SoftafriqueBackup`,
}

// The shipped example and the installer template must survive strict parsing.
// A fresh install or a copy-pasted example that fails at startup is the worst
// possible outcome for a change made to catch exactly that class of mistake.
func TestShippedConfigsLoad(t *testing.T) {
	for _, path := range []string{
		"../../config.example.yaml",
		"../../installer/config.yaml.template",
	} {
		cfg := Default()
		if err := loadYAML(substitute(readFile(t, path)), cfg); err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s: validate: %v", path, err)
		}
	}
}

// The opt-in is written by install.ps1 rather than by the MSI, so the two halves
// of the network-share rule -- the installer's check and the agent's -- are
// decided by one switch but recorded in two places. This pins the half that is
// easy to forget: the agent honouring the key when it is set, and refusing when
// it is not.
func TestTheOptInIsOffInEveryShippedConfig(t *testing.T) {
	for _, path := range []string{
		"../../config.example.yaml",
		"../../installer/config.yaml.template",
	} {
		cfg := Default()
		if err := loadYAML(substitute(readFile(t, path)), cfg); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if cfg.AllowUNC {
			t.Errorf("%s ships with allow_unc on; a network share must be asked for, not defaulted to", path)
		}
	}
}

// substitute fills the placeholders the way the MSI's ConfigurableTextFile does.
func substitute(body []byte) []byte {
	out := string(body)
	for name, value := range templateValues {
		out = strings.ReplaceAll(out, name, value)
	}
	return []byte(out)
}
