package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"softafrique-backup-agent/internal/agent"
	"softafrique-backup-agent/internal/status"
)

// The enrollment token is a credential: it must not be readable out of the
// process table, and it must not be left in a world-readable file.
func TestResolveTokenPrefersTheEnvironment(t *testing.T) {
	t.Setenv(EnrollTokenEnv, "tok-from-env")
	got, fromFlag, err := resolveToken("tok-from-flag", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "tok-from-env" {
		t.Errorf("token = %q, want the environment to win over the flag", got)
	}
	if fromFlag {
		t.Error("a token from the environment is not from the command line and must not be flagged as such")
	}
}

func TestResolveTokenRejectsAnEmptyToken(t *testing.T) {
	t.Setenv(EnrollTokenEnv, "")
	if _, _, err := resolveToken("  ", ""); err == nil {
		t.Error("an empty token must be refused rather than sent to the gateway")
	}
}

func TestResolveTokenReadsAProtectedFile(t *testing.T) {
	t.Setenv(EnrollTokenEnv, "")
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("tok-abc123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, fromFlag, err := resolveToken("", path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "tok-abc123" {
		t.Errorf("token = %q; surrounding whitespace should be trimmed", got)
	}
	if fromFlag {
		t.Error("a file is not the command line")
	}
}

func TestResolveTokenRefusesAWorldReadableFile(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("file modes are not enforced for root")
	}
	t.Setenv(EnrollTokenEnv, "")
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("tok-abc123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := resolveToken("", path)
	if err == nil {
		t.Fatal("a token file other users can read is a token in the clear")
	}
	if !strings.Contains(err.Error(), "readable by other users") {
		t.Errorf("error = %q, want it to explain the problem", err)
	}
}

func TestResolveTokenRejectsAnEmptyFile(t *testing.T) {
	t.Setenv(EnrollTokenEnv, "")
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveToken("", path); err == nil {
		t.Error("an empty token file must be refused")
	}
}

// The exit code is what an RMM task result is built from, so the mapping has to
// distinguish "not enrolled yet" from "broken".
func TestExitCodesAreDistinct(t *testing.T) {
	if got := exitCodeFor(nil); got != exitOK {
		t.Errorf("nil error gave %d", got)
	}
	if got := exitCodeFor(agent.ErrNotEnrolled); got != exitNotEnrolled {
		t.Errorf("ErrNotEnrolled gave %d, want %d", got, exitNotEnrolled)
	}
	if got := exitCodeFor(errors.New("boom")); got != exitError {
		t.Errorf("a plain error gave %d, want %d", got, exitError)
	}
	if got := exitCodeFor(actionRequired("re-enroll the device")); got != exitActionRequired {
		t.Errorf("an action-required error gave %d, want %d", got, exitActionRequired)
	}
}

// A device that has never had a successful backup must not be reported as
// healthy, and one that has not backed up in two intervals should not either.
func TestShortStatusIsOneLineAndHasNoSecrets(t *testing.T) {
	line := shortStatus(&statusFixture)
	if strings.Contains(line, "\n") {
		t.Error("the short status must be a single line for grep-style matching")
	}
	for _, field := range []string{"enrolled=", "last_attempt=", "last_success=", "consecutive_failures=", "gateway="} {
		if !strings.Contains(line, field) {
			t.Errorf("short status is missing %q: %s", field, line)
		}
	}
}

// orDash keeps the short status parseable when a field is unset.
func TestOrDash(t *testing.T) {
	if orDash("") != "-" {
		t.Error("an empty field should render as a dash, not as an empty value")
	}
	if orDash("active") != "active" {
		t.Error("a set field should render as itself")
	}
}

// statusFixture is a device mid-life: enrolled, one success, one failure since.
var statusFixture = status.Status{
	SchemaVersion:       status.SchemaVersion,
	DeviceID:            "dev-01HQ8",
	Tenant:              "acme",
	Enrolled:            true,
	ServerStatus:        "active",
	LastAttemptStatus:   status.AttemptFailed,
	LastSuccess:         "2026-09-26T04:00:00Z",
	ConsecutiveFailures: 1,
	RepoStats:           &status.RepoStats{RepoBytes: 4096, SnapshotCount: 12},
}

// The token is a one-time secret: whoever reads it can enroll a machine of their
// own as this customer. A file other accounts can read has to be refused.
func TestCheckTokenFileProtection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.txt")
	if err := os.WriteFile(path, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkTokenFileProtection(path); err != nil {
		t.Errorf("a 0600 file must be accepted: %v", err)
	}

	// A new file: os.WriteFile on an existing file would keep the old mode.
	loose := filepath.Join(dir, "loose.txt")
	if err := os.WriteFile(loose, []byte("tok"), 0o644); err != nil {
		t.Fatal(err)
	}
	path = loose
	if runtime.GOOS != "windows" {
		err := checkTokenFileProtection(path)
		if err == nil {
			t.Fatal("a world-readable token file must be refused")
		}
		if !strings.Contains(err.Error(), "icacls") {
			t.Errorf("the error must tell an operator how to fix it, got %q", err)
		}
	}
}

// A missing file is a mistake in the command, not a permissions problem.
func TestCheckTokenFileProtectionMissing(t *testing.T) {
	if err := checkTokenFileProtection(filepath.Join(t.TempDir(), "nope.txt")); err == nil {
		t.Fatal("expected an error for a missing token file")
	}
}

// Only the installing principals may read a token file. A file shared with
// "Users" or "Everyone" is the exact case this has to catch.
func TestIsPrivilegedTokenReader(t *testing.T) {
	me := currentUserName()
	for _, who := range []string{me, "NT AUTHORITY\\SYSTEM", "BUILTIN\\Administrators", "Administrators"} {
		if !isPrivilegedTokenReader(who) {
			t.Errorf("%q should be allowed to read a token file", who)
		}
	}
	for _, who := range []string{"BUILTIN\\Users", "Everyone", "NT AUTHORITY\\NETWORK SERVICE", "some-desk-helpdesk"} {
		if isPrivilegedTokenReader(who) {
			t.Errorf("%q must not be allowed to read a token file", who)
		}
	}
}
