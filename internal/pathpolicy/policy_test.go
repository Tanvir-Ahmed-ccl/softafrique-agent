package pathpolicy

import (
	"errors"
	"strings"
	"testing"
)

// The share test decides whether the agent is allowed to touch a remote machine,
// so a false positive refuses a customer's local data folder and a false negative
// lets the service authenticate to a share nobody approved. Both are worth a
// table.
func TestIsUncTellsSharesFromOrdinaryPaths(t *testing.T) {
	shares := []string{
		`\\fileserver\CustomerData`,
		`\\server\share`,
		`\\SERVER01\Backup\Customer`,
		`  \\server\share\folder  `,
	}
	for _, p := range shares {
		if !IsUNC(p) {
			t.Errorf("IsUNC(%q) = false, want true", p)
		}
	}
	// The drive-letter cases are the ones a text test gets wrong: C:\ and
	// \\server\share both contain a backslash, so testing for separators alone
	// calls every path a share.
	notShares := []string{
		`C:\SoftafriqueBackup`,
		`D:\Customer\Data`,
		`C:\`,
		`\\`,
		`\folder`,
		``,
		`C:\\not\share`,
	}
	for _, p := range notShares {
		if IsUNC(p) {
			t.Errorf("IsUNC(%q) = true, want false", p)
		}
	}
}

// The share half of the rule runs on every platform, so it is testable anywhere.
// The volume half needs Windows and lives in volume_windows_test.go.
func TestRequireFixedRefusesAShareUnlessOptedIn(t *testing.T) {
	const share = `\\fileserver\CustomerData`

	err := RequireFixed(share, false, AllowUNCFlag)
	if err == nil {
		t.Fatal("a share was allowed with no opt-in")
	}
	var perr *Error
	if !errors.As(err, &perr) {
		t.Fatalf("err is %T, want *Error", err)
	}
	if perr.Kind != KindNetwork {
		t.Errorf("Kind = %v, want KindNetwork", perr.Kind)
	}
	if perr.OptIn != AllowUNCFlag {
		t.Errorf("OptIn = %q, want %q: the refusal has to say how to proceed", perr.OptIn, AllowUNCFlag)
	}

	if err := RequireFixed(share, true, AllowUNCFlag); err != nil {
		t.Errorf("a share with the opt-in was refused: %v", err)
	}
}

// An empty path is not a share, and must not be reported as one. The agent
// filters empty paths before calling, but the rule should be harmless on its own.
func TestAnEmptyPathIsNotAShare(t *testing.T) {
	if IsUNC("   ") {
		t.Error("whitespace was read as a share")
	}
}

func TestIsUnmountedOnlyMatchesAnEmptyVolume(t *testing.T) {
	if IsUnmounted(&Error{Path: `D:\Data`, Kind: KindOptical}) {
		t.Error("an optical drive was reported as an unmounted drive")
	}
	if !IsUnmounted(&Error{Path: `Z:\Data`, Kind: KindNoRoot}) {
		t.Error("an empty drive letter was not reported as unmounted")
	}
	if IsUnmounted(errors.New("something else")) {
		t.Error("an unrelated error was reported as unmounted")
	}
}

// Every refusal has to name the path and say what kind of volume it landed on. A
// status file that says "policy violation" is a support ticket nobody can close.
//
// This calls decide rather than RequireFixed because decide is the whole rule and
// has no platform in it, so the assertion runs on a developer's macOS machine and
// not only on the Windows CI runner. It is here because that is exactly what was
// missing: the unmounted-drive message built its own wording and never went
// through Kind.String(), so the defect could not be seen until a Windows job ran
// it. One kind getting its own sentence is how that happened.
func TestEveryRefusalNamesThePathAndTheVolume(t *testing.T) {
	const path = `D:\Customer\Data`
	for kind := KindUnknown; kind <= KindNoRoot; kind++ {
		if kind == KindFixed {
			continue
		}
		err := decide(path, kind, false, AllowUNCFlag)
		if err == nil {
			t.Errorf("%v was allowed; only a fixed disk is", kind)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, path) {
			t.Errorf("%v: message %q does not name the path", kind, msg)
		}
		if !strings.Contains(msg, kind.String()) {
			t.Errorf("%v: message %q does not say what the volume is", kind, msg)
		}
		// A gap in the sentence is how "is on drive , which is not mounted" shipped:
		// the kind is present but something interpolated to nothing beside it.
		if strings.Contains(msg, "  ") || strings.Contains(msg, "is on ;") {
			t.Errorf("%v: message %q has a hole in it", kind, msg)
		}
	}
}

// The opt-in exists for a network share and for nothing else. This is the test
// that keeps -allow-unc from turning into a general "accept any volume" switch,
// which is how a DVD drive gets accepted and then fails every backup with "device
// is not ready" hours later.
//
// A fixed disk stays allowed, because the switch is not what allows it: it would
// be allowed with the switch off too.
func TestTheShareOptInDoesNotAllowOtherVolumeTypes(t *testing.T) {
	const path = `D:\Customer\Data`
	for kind := KindUnknown; kind <= KindNoRoot; kind++ {
		if kind == KindNetwork || kind == KindFixed {
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
	if err := decide(path, KindFixed, false, AllowUNCFlag); err != nil {
		t.Errorf("a fixed disk was refused with the opt-in off: %v", err)
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

// Only a fixed disk is allowed, and this asks the rule rather than the machine: a
// test that says "D: is refused" passes on a laptop with no DVD drive and fails on
// a build server where D: is a second fixed disk. decide takes the classification
// as an argument precisely so the table can be asserted without asking the machine
// what drives it happens to have.
//
// The whole table, because the original bug was a type nobody had thought about.
func TestOnlyAFixedDiskIsAllowed(t *testing.T) {
	const path = `X:\Customer\Data`
	for kind := KindUnknown; kind <= KindNoRoot; kind++ {
		err := decide(path, kind, false, AllowUNCFlag)
		if kind == KindFixed {
			if err != nil {
				t.Errorf("a fixed disk was refused: %v", err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%v was allowed; the folder to protect must be on a fixed disk", kind)
		}
	}
}
