package pathpolicy

import (
	"errors"
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
