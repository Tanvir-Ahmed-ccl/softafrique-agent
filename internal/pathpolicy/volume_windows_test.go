//go:build windows

package pathpolicy

import (
	"errors"
	"testing"
)

// The volume half of the rule needs Windows, so this is the only place it is
// asserted at all. CI builds and runs it on a Windows runner; on a developer
// machine this file does not exist, which is why policy_test.go carries the share
// half and says so.
//
// The assertions go through decide rather than through RequireFixed with real
// paths. "D: is refused" would pass on a laptop with no DVD drive and fail on a
// build server where D: is a second fixed disk, so a test written that way is a
// test about the machine. This one is about the rule.

// The whole table, because the original bug was a type nobody had thought about.
func TestOnlyAFixedDiskIsAllowed(t *testing.T) {
	const path = `X:\Customer\Data`
	allowed := map[Kind]bool{KindFixed: true}
	for kind := KindUnknown; kind <= KindNoRoot; kind++ {
		err := decide(path, kind, false, AllowUNCFlag)
		if allowed[kind] {
			if err != nil {
				t.Errorf("%v was refused: %v", kind, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%v was allowed; the folder to protect must be on a fixed disk", kind)
		}
	}
}

// The opt-in exists for a network share and for nothing else. This is the test
// that keeps -allow-unc from turning into a general "accept any volume" switch,
// which is how a DVD drive gets accepted and then fails every backup with "device
// is not ready" hours later.
func TestTheShareOptInDoesNotAllowOtherVolumeTypes(t *testing.T) {
	const path = `D:\Customer\Data`
	for kind := KindUnknown; kind <= KindNoRoot; kind++ {
		if kind == KindNetwork {
			continue
		}
		if err := decide(path, kind, true, AllowUNCFlag); err == nil {
			t.Errorf("%v was allowed with the share opt-in set", kind)
		}
	}
	if err := decide(path, KindNetwork, true, AllowUNCFlag); err != nil {
		t.Errorf("a share with the opt-in was refused: %v", err)
	}
	if err := decide(path, KindNetwork, false, AllowUNCFlag); err == nil {
		t.Error("a share without the opt-in was allowed")
	}
}

// A drive letter with no volume is a mistyped letter, and gets the kind that says
// so, because "Z: is not a drive" and "that is a DVD drive" are different
// mistakes with different fixes.
func TestADriveLetterWithNoVolumeIsReportedAsUnmounted(t *testing.T) {
	err := decide(`Z:\Customer\Data`, KindNoRoot, false, AllowUNCFlag)
	var perr *Error
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want a *Error", err)
	}
	if !IsUnmounted(err) {
		t.Error("an unmounted drive letter was not reported as unmounted")
	}
	// There is no opt-in for a drive that is not there, so offering one would tell
	// somebody to pass a flag that cannot help.
	if perr.OptIn != "" {
		t.Errorf("OptIn = %q, want empty: no switch fixes a drive letter that is not mounted", perr.OptIn)
	}
}

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
