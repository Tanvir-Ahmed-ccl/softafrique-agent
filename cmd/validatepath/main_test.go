package main

import (
	"os"
	"path/filepath"
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
		{"missing parent", []string{filepath.Join(dir, "nope", "deeper")}, 3},
		{"not a directory", []string{file}, 4},
		{"good", []string{good}, 0},
		{"not there yet but the parent is", []string{filepath.Join(dir, "later")}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validate(tc.args); got != tc.want {
				t.Errorf("validate(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

// Backing up a whole drive by accident is expensive and looks like an attack,
// so a volume root is refused.
func TestValidateRefusesAVolumeRoot(t *testing.T) {
	if !isVolumeRoot(string(filepath.Separator)) {
		t.Error("the filesystem root should be recognised as a volume root")
	}
	if isVolumeRoot("/tmp") {
		t.Error("an ordinary directory is not a volume root")
	}
}
