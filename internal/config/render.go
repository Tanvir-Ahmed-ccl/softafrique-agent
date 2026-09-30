package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The installer writes config.yaml from installer/config.yaml.template, filling
// in the paths that are only known at install time.
//
// This substitution used to be done by the MSI, by way of the Util extension's
// ConfigurableTextFile. WiX 4 deleted that element and WiX 5 has no replacement:
// util:XmlConfig is the closest thing left and it rewrites XML, not YAML, so it
// is not usable here. The substitution is therefore done by the agent itself,
// which is the binary the MSI already ships and already runs during install.
//
// It was worth moving the logic into Go rather than dropping the placeholders,
// because the result is the file every install boots from, and a template that
// survives substitution on a developer's machine and not on a technician's is
// exactly the failure this is meant to prevent.

// placeholder matches the template's placeholders: an upper-case name in square
// brackets. The shipped template contains no other bracketed token, so anything
// this matches after substitution is a name that was typed wrong.
var placeholder = regexp.MustCompile(`\[[A-Z][A-Z0-9_]*\]`)

// RenderOptions are the values substituted into a config template. The three
// state paths default to the standard locations, which is what the MSI used to
// write; a deployment that relocates the data directory can override them.
type RenderOptions struct {
	BackupPath string
	DataDir    string
	StatusFile string
	LogFile    string
}

func (o RenderOptions) withDefaults() (RenderOptions, error) {
	if strings.TrimSpace(o.BackupPath) == "" {
		return o, fmt.Errorf("backup path is required; the MSI did not supply BACKUPPATH")
	}
	def := Default()
	if o.DataDir == "" {
		o.DataDir = def.DataDir
	}
	if o.StatusFile == "" {
		o.StatusFile = def.StatusFile
	}
	if o.LogFile == "" {
		o.LogFile = def.LogFile
	}
	return o, nil
}

// Render fills a config template and returns the result. It refuses to return a
// document that still contains a placeholder, because the agent would then
// parse a path called "[BACKUPPATH]" as if it were a real one.
func Render(body []byte, opts RenderOptions) ([]byte, error) {
	opts, err := opts.withDefaults()
	if err != nil {
		return nil, err
	}

	out := string(body)
	for name, value := range map[string]string{
		"[BACKUPPATH]": opts.BackupPath,
		"[DATADIR]":    opts.DataDir,
		"[STATUSFILE]": opts.StatusFile,
		"[LOGFILE]":    opts.LogFile,
	} {
		out = strings.ReplaceAll(out, name, value)
	}

	if left := placeholder.FindAllString(out, -1); len(left) > 0 {
		return nil, fmt.Errorf("template still contains %s after substitution; "+
			"check the spelling against config.RenderOptions", strings.Join(left, " "))
	}

	// Parse what was just rendered. The install is the last cheap moment to
	// notice that a substitution produced a document the agent cannot load; on
	// the next boot the service would start, fail to parse, and stop, and the
	// first sign of it would be a device in the RMM with no status.
	cfg := Default()
	if err := loadYAML([]byte(out), cfg); err != nil {
		return nil, fmt.Errorf("rendered config does not parse: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("rendered config is not valid: %w", err)
	}
	if err := statusAndLogUnderDataDir(cfg); err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// RenderToFile renders a template to a path. It does not overwrite an existing
// file, which is what the MSI's ConfigurableTextFile NeverOverwrite attribute
// did: a repair, or an upgrade by a new major version, must not discard a
// config a technician edited on site. It reports whether it wrote anything.
func RenderToFile(template, out string, opts RenderOptions, force bool) (bool, error) {
	if !force {
		if _, err := os.Stat(out); err == nil {
			return false, nil
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}

	body, err := os.ReadFile(template)
	if err != nil {
		return false, err
	}
	rendered, err := Render(body, opts)
	if err != nil {
		return false, err
	}
	if err := writeFileAtomic(out, rendered); err != nil {
		return false, err
	}
	return true, nil
}

// writeFileAtomic writes via a temporary file in the same directory and renames
// over the target, so a failure part-way through cannot leave a half-written
// config where the agent will read it.
func writeFileAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// os.Rename maps to MoveFileEx with MOVEFILE_REPLACE_EXISTING on Windows,
	// so this replaces the target rather than failing on it.
	return os.Rename(name, path)
}

// statusAndLogUnderDataDir reports the invariant the installer relies on: the
// status file and the log live in the data directory the config names. It is
// checked here rather than in the MSI because a mismatch is only visible after
// the paths are joined, and a mismatch means the agent writes its status
// somewhere nobody is monitoring.
func statusAndLogUnderDataDir(c *Config) error {
	if c.DataDir == "" {
		return nil
	}
	for _, f := range []struct{ name, path string }{
		{"status_file", c.StatusFile},
		{"log_file", c.LogFile},
	} {
		if f.path == "" {
			continue
		}
		if !underDir(c.DataDir, f.path) {
			return fmt.Errorf("%s %q is outside data_dir %q", f.name, f.path, c.DataDir)
		}
	}
	return nil
}

// underDir reports whether path sits inside dir. It compares on separators
// rather than using filepath, because the values are Windows paths being
// checked by a build that may not be running on Windows, and a check that
// silently changes meaning with the host is worse than no check.
func underDir(dir, path string) bool {
	norm := func(s string) string {
		return strings.ToLower(strings.ReplaceAll(s, `\`, "/"))
	}
	d := strings.TrimRight(norm(dir), "/")
	return strings.HasPrefix(norm(path), d+"/")
}
