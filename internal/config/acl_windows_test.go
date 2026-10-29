//go:build windows

package config

import (
	"os/exec"
	"strings"
	"testing"
)

// The installer used to grant explicit SYSTEM/Administrators ACEs without
// breaking inheritance, leaving BUILTIN\Users:(RX) on the credentials folder.
// The runtime check has to see that, or the warning is just a comment.
func TestDataDirACLWarningNamesUsersWhoCanReadTheCredentials(t *testing.T) {
	dir := t.TempDir()
	out, err := exec.Command("icacls", dir, "/grant", `*S-1-5-32-545:(OI)(CI)(RX)`).CombinedOutput()
	if err != nil {
		t.Skipf("could not widen the ACL to test the Windows path: %v: %s", err, out)
	}

	warn := dataDirACLWarning(dir)
	if warn == "" {
		t.Fatal("a data directory readable by Users produced no warning")
	}
	if !strings.Contains(warn, "icacls") {
		t.Errorf("warning %q does not tell an operator how to fix it", warn)
	}
}

func TestTrustedDataDirReader(t *testing.T) {
	for _, sid := range []string{"S-1-5-18", "s-1-5-32-544"} {
		if !trustedDataDirReader(sid) {
			t.Errorf("%q should be trusted", sid)
		}
	}
	for _, sid := range []string{"S-1-5-32-545", "S-1-1-0", "S-1-5-11"} {
		if trustedDataDirReader(sid) {
			t.Errorf("%q must not be trusted", sid)
		}
	}
}
