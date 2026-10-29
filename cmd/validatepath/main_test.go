package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The helper's exit code is the MSI's only signal, so each failure mode needs
// its own code rather than a generic 1.
func TestValidateExitCodes(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "CustomerData")
	if err := os.Mkdir(good, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"empty", []string{""}, 2},
		{"not a directory", []string{file}, 4},
		{"good", []string{good}, 0},
		{"a volume root", []string{string(filepath.Separator)}, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validate(tc.args); got != tc.want {
				t.Errorf("validate(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

// A customer folder that does not exist yet is not a reason to refuse the
// install, but the validator must not create it. Creating it here gives the
// folder the installing user's owner and ACL, and makes the second run answer a
// different question from the first.
func TestValidateAcceptsAMissingFolderWithoutCreatingIt(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "CustomerData")

	if got := validate([]string{target}); got != 0 {
		t.Fatalf("validate = %d, want 0", got)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("the folder was created; validation must have no side effect: %v", err)
	}

	// Running it twice must be harmless: the MSI can call it more than once, and
	// a repair re-runs the execute sequence.
	if got := validate([]string{target}); got != 0 {
		t.Errorf("second validate = %d, want 0", got)
	}
}

// BACKUPPATH is routinely given as D:\Customer\Data where neither level exists.
// The missing tree is still accepted, without being built.
func TestValidateAcceptsMissingIntermediateFolders(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "Customer", "Data")

	if got := validate([]string{target}); got != 0 {
		t.Fatalf("validate = %d, want 0", got)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("the nested folder was created: %v", err)
	}
}

// A file sitting where the folder should be must not be "fixed" by deleting it.
func TestValidateRefusesToOverwriteAFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-folder")
	if err := os.WriteFile(file, []byte("customer data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := validate([]string{file}); got != 4 {
		t.Fatalf("validate = %d, want 4", got)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("the file was removed; validatepath must never delete anything")
	}
}

// BACKUPPATH is typed by a technician, so flag parsing has to be narrow enough
// that a folder name is never mistaken for a switch. Only -allow-unc is a flag,
// and only the path argument survives everything else.
func TestOnlyTheOptInFlagIsParsedAsAFlag(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		allowUNC bool
		path     string
	}{
		{"plain path", []string{`C:\Customer Data`}, false, `C:\Customer Data`},
		{"flag first", []string{allowUNCFlag, `C:\CustomerData`}, true, `C:\CustomerData`},
		{"flag last", []string{`C:\CustomerData`, allowUNCFlag}, true, `C:\CustomerData`},
		{"case insensitive", []string{"-ALLOW-UNC", `C:\CustomerData`}, true, `C:\CustomerData`},
		{"a dash in the name is not a flag", []string{`-customer`, `-data`}, false, `-customer -data`},
		{"empty", []string{""}, false, ""},
		{"no arguments at all", nil, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allowUNC, path := parseArgs(tc.args)
			if allowUNC != tc.allowUNC {
				t.Errorf("allowUNC = %v, want %v", allowUNC, tc.allowUNC)
			}
			if path != tc.path {
				t.Errorf("path = %q, want %q", path, tc.path)
			}
		})
	}
}

// Backing up a whole drive by accident is expensive and looks like an attack,
// so a volume root is refused.
func TestValidateRefusesAVolumeRoot(t *testing.T) {
	if !isVolumeRoot(filepath.VolumeName(t.TempDir()) + string(filepath.Separator)) {
		if runtime.GOOS == "windows" {
			t.Error("a Windows drive root should be recognised as a volume root")
		}
	}
	if isVolumeRoot(t.TempDir()) {
		t.Error("an ordinary directory is not a volume root")
	}
}

// The system-directory refusal is a Windows-path rule, so it is exercised on the
// platform where it matters. The rule itself is tested cross-platform in
// internal/pathpolicy.
func TestValidateRefusesASystemDirectory(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("C:\\Windows is only an actual directory on Windows")
	}
	for _, path := range []string{`C:\Windows`, `C:\Program Files`, `C:\ProgramData`} {
		if got := validate([]string{path}); got != 5 {
			t.Errorf("validate(%q) = %d, want 5", path, got)
		}
	}
}

// \\server\share is a volume to Windows but a share root to a person, and must
// not be described as a drive root.
func TestShareRootIsRecognised(t *testing.T) {
	for _, path := range []string{`\\server\share`, `\\server\share\`, `//server/share`} {
		if !isShareRoot(path) {
			t.Errorf("%q should be a share root", path)
		}
	}
	for _, path := range []string{`\\server\share\folder`, `C:\`, `/tmp`} {
		if isShareRoot(path) {
			t.Errorf("%q is not a share root", path)
		}
	}
}
