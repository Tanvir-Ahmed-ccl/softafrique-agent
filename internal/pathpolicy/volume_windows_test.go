//go:build windows

package pathpolicy

import "testing"

// What is left here is genuinely about Windows: reading a drive type, and turning
// a path into the root string GetDriveType wants.
//
// The rule itself -- which volume types are allowed, what each refusal says, and
// what the share opt-in does and does not allow -- is asserted in policy_test.go,
// which is not platform-gated. That is not tidiness. decide takes the
// classification as an argument, so those assertions have no platform in them, and
// leaving them here meant a mistake in them could not be seen until a Windows job
// ran: a message that dropped the volume, and a test that required a fixed disk to
// be refused, both reached a tagged build that way.

// A UNC path is caught by the textual test before the drive table is consulted,
// which is what lets the share half work on a machine where the share is
// unreachable. Confirming the two agree stops a refactor dropping one of them.
func TestVolumeRootFindsTheShareRoot(t *testing.T) {
	cases := map[string]string{
		`\\server\share\folder`: `\\server\share`,
		`\\server\share`:        `\\server\share`,
		`C:\Customer\Data`:      `C:\`,
		`C:\`:                   `C:\`,
	}
	for path, want := range cases {
		if got := volumeRoot(path); got != want {
			t.Errorf("volumeRoot(%q) = %q, want %q", path, got, want)
		}
	}
}

// The classification itself, against the drives that exist on the machine running
// the test. Not a table, because the drives are the input: this only has to hold
// for whatever C: happens to be, which on any machine that has a system disk is
// a fixed disk.
func TestClassifyVolumeReadsTheSystemDiskAsFixed(t *testing.T) {
	kind, decidable := classifyVolume(`C:\Customer\Data`)
	if !decidable {
		t.Fatal("Windows could not classify a volume")
	}
	if kind != KindFixed {
		t.Errorf("C: classified as %v, want a fixed disk", kind)
	}
}

// Windows can always classify, unlike every other platform, so a caller relying on
// the "cannot tell" path gets an answer it did not ask for. The shipped build is
// the one that matters, and it is this one.
func TestClassifyVolumeIsAlwaysDecidableOnWindows(t *testing.T) {
	for _, path := range []string{`C:\Customer\Data`, `Z:\Customer\Data`, `\\server\share\x`, `relative\path`, ``} {
		if _, decidable := classifyVolume(path); !decidable {
			t.Errorf("classifyVolume(%q) reported it could not tell", path)
		}
	}
}
