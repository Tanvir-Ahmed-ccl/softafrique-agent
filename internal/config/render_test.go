package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderFillsTheInstallerTemplate(t *testing.T) {
	out, err := Render(readFile(t, "../../installer/config.yaml.template"), RenderOptions{
		BackupPath: `C:\Customers\Acme Documents`,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// The template must not survive into the installed file, and the path must
	// survive intact: a backup path with a space in it is ordinary and the
	// config quotes it.
	if strings.Contains(string(out), "[BACKUPPATH]") {
		t.Error("BACKUPPATH placeholder left in the rendered config")
	}
	if !strings.Contains(string(out), `C:\Customers\Acme Documents`) {
		t.Error("backup path not written into the rendered config")
	}
	if got := string(out); !strings.Contains(got, "data_dir:") {
		t.Error("rendered config lost data_dir")
	}
}

func TestRenderRefusesWithoutABackupPath(t *testing.T) {
	_, err := Render(readFile(t, "../../installer/config.yaml.template"), RenderOptions{})
	if err == nil {
		t.Fatal("a config with no backup path was rendered; install would ship an agent that protects nothing")
	}
	if !strings.Contains(err.Error(), "backup path") {
		t.Errorf("error should name the missing value, got %v", err)
	}
}

func TestRenderRefusesALeftoverPlaceholder(t *testing.T) {
	// A typo in a placeholder name is the failure this guard exists for. It
	// produces a document that parses, because "[BACKUPPAT]" is just a quoted
	// string, and that is exactly why it needs catching here.
	body := []byte("version: 2\ninclude:\n  - '[BACKUPPAT]'\ndata_dir: '[DATADIR]'\n")
	_, err := Render(body, RenderOptions{BackupPath: `C:\Data`})
	if err == nil {
		t.Fatal("a misspelled placeholder was rendered; the agent would back up a folder literally named [BACKUPPAT]")
	}
	if !strings.Contains(err.Error(), "[BACKUPPAT]") {
		t.Errorf("error should name the leftover placeholder, got %v", err)
	}
}

func TestRenderRejectsAConfigThatDoesNotParse(t *testing.T) {
	// Substituting into a broken template must fail the install, not produce a
	// config the service discovers is broken on its first boot.
	body := []byte("version: 2\ninclude:\n  - '[BACKUPPATH]'\ndata_dir: '[DATADIR]'\n  bad indent: [\n")
	if _, err := Render(body, RenderOptions{BackupPath: `C:\Data`}); err == nil {
		t.Fatal("Render accepted a template that does not parse")
	}
}

// A status file outside data_dir is the one substitution mistake that is
// invisible in the file itself: the config parses, the agent runs, and the
// status lands somewhere no monitor is looking.
func TestRenderRejectsAStatusFileOutsideTheDataDir(t *testing.T) {
	body, err := Render([]byte("version: 2\ninclude:\n  - '[BACKUPPATH]'\ndata_dir: '[DATADIR]'\n"),
		RenderOptions{
			BackupPath: `C:\Data`,
			DataDir:    `C:\ProgramData\Agent`,
			StatusFile: `C:\Windows\Temp\status.json`,
		})
	if err == nil {
		t.Fatalf("accepted a status file outside the data dir:\n%s", body)
	}
	if !strings.Contains(err.Error(), "status_file") {
		t.Errorf("error should name status_file, got %v", err)
	}
}

// underDir has to give the same answer on a macOS build checking a Windows
// config as it does on Windows, or this guard is only enforced where it is
// least likely to be needed.
func TestUnderDirIsHostIndependent(t *testing.T) {
	cases := []struct {
		dir, path string
		want      bool
	}{
		{`C:\ProgramData\Agent`, `C:\ProgramData\Agent\status.json`, true},
		{`C:\ProgramData\Agent`, `C:\ProgramData\Agent\sub\status.json`, true},
		{`C:\ProgramData\Agent`, `C:\Windows\Temp\status.json`, false},
		{`C:\ProgramData\Agent`, `C:\ProgramData\AgentOther\status.json`, false},
		{`/var/lib/agent`, `/var/lib/agent/status.json`, true},
		{`/var/lib/agent`, `/var/lib/other/status.json`, false},
		{`/var/lib/agent`, `C:\ProgramData\Agent\status.json`, false},
	}
	for _, c := range cases {
		if got := underDir(c.dir, c.path); got != c.want {
			t.Errorf("underDir(%q, %q) = %v, want %v", c.dir, c.path, got, c.want)
		}
	}
}

func TestRenderToFileDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	template := filepath.Join(dir, "template.yaml")
	out := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(template, []byte("version: 2\ninclude:\n  - '[BACKUPPATH]'\ndata_dir: '[DATADIR]'\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// What a technician typed on site.
	edited := "version: 2\ninclude:\n  - 'C:\\Corrected By Technician'\ndata_dir: 'C:\\ProgramData\\SoftafriqueBackupAgent'\n"
	if err := os.WriteFile(out, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	// A repair or an upgrade runs this again and must not undo the edit.
	wrote, err := RenderToFile(template, out, RenderOptions{BackupPath: `C:\FromMsi`}, false)
	if err != nil {
		t.Fatalf("RenderToFile: %v", err)
	}
	if wrote {
		t.Error("RenderToFile overwrote an existing config; an upgrade would discard on-site changes")
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != edited {
		t.Errorf("config was modified:\n got %q\nwant %q", got, edited)
	}
}

func TestRenderToFileWritesAndForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	template := filepath.Join(dir, "template.yaml")
	out := filepath.Join(dir, "config.yaml")
	body := []byte("version: 2\ninclude:\n  - '[BACKUPPATH]'\ndata_dir: '[DATADIR]'\n")
	if err := os.WriteFile(template, body, 0o644); err != nil {
		t.Fatal(err)
	}

	wrote, err := RenderToFile(template, out, RenderOptions{BackupPath: `C:\First`}, false)
	if err != nil || !wrote {
		t.Fatalf("RenderToFile: wrote=%v err=%v", wrote, err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "[BACKUPPATH]") {
		t.Errorf("placeholder left in written config:\n%s", got)
	}

	wrote, err = RenderToFile(template, out, RenderOptions{BackupPath: `C:\Second`}, true)
	if err != nil || !wrote {
		t.Fatalf("force RenderToFile: wrote=%v err=%v", wrote, err)
	}
	got, err = os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `C:\Second`) {
		t.Errorf("force did not overwrite:\n%s", got)
	}

	// No temporary file may be left lying next to the config.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		t.Errorf("stray file left behind: %s", e.Name())
	}
}

// The shipped-template test used to substitute by hand, which proved the test
// harness worked rather than the installer. Point it at the real code path.
func TestShippedTemplateRenders(t *testing.T) {
	out, err := Render(readFile(t, "../../installer/config.yaml.template"), RenderOptions{
		BackupPath: `C:\SoftafriqueBackup`,
	})
	if err != nil {
		t.Fatalf("the shipped template does not render: %v", err)
	}
	if strings.Contains(string(out), "[") && placeholder.Match(out) {
		t.Errorf("rendered template still contains a placeholder:\n%s", out)
	}
}
