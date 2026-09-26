package secret

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testCreds() *Credentials {
	return &Credentials{
		DeviceID:       "dev-01HQ8XK3M4N7",
		DevicePassword: "9f2c4a7b1d8e",
		EncryptionKey:  "e3b1f0a95c2d47e6b8a10f34c9d2e7b56",
		Repository:     "rest:https://backup.softafrique.net/dev-01HQ8XK3M4N7",
		Tenant:         "softafrique",
		BackupPath:     `C:\SoftafriqueBackup`,
		Server:         "https://backup.softafrique.net/api/v1",
		EnrolledAt:     "2026-09-26T09:00:00Z",
	}
}

func TestStoreRoundTrip(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "credentials.dat"))
	if s.Exists() {
		t.Fatal("fresh store should not exist")
	}
	if _, err := s.Load(); err != ErrNotFound {
		t.Fatalf("want ErrNotFound on a fresh store, got %v", err)
	}

	want := testCreds()
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	if !s.Exists() {
		t.Fatal("store should exist after save")
	}

	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.DeviceID != want.DeviceID || got.DevicePassword != want.DevicePassword ||
		got.EncryptionKey != want.EncryptionKey || got.Repository != want.Repository ||
		got.Tenant != want.Tenant {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got.Redacted(), want.Redacted())
	}
}

func TestSaveRejectsIncompleteCredentials(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "credentials.dat"))
	cases := map[string]func(*Credentials){
		"no device id":  func(c *Credentials) { c.DeviceID = "" },
		"no password":   func(c *Credentials) { c.DevicePassword = "" },
		"no key":        func(c *Credentials) { c.EncryptionKey = "" },
		"no repository": func(c *Credentials) { c.Repository = "" },
		"creds in repo": func(c *Credentials) { c.Repository = "rest:https://user:pass@backup.softafrique.net/x" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := testCreds()
			mutate(c)
			if err := s.Save(c); err == nil {
				t.Fatal("expected validation to reject this credential set")
			}
			if s.Exists() {
				t.Fatal("rejected credentials must not be written to disk")
			}
		})
	}
}

func TestBlobDoesNotContainPlaintextSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.dat")
	s := NewStore(path)
	c := testCreds()
	if err := s.Save(c); err != nil {
		t.Fatal(err)
	}
	// On Windows the blob is DPAPI-sealed so this is a real assertion. On other
	// platforms protect() is a no-op and the test only documents the weaker
	// guarantee, so skip the secret check there.
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if isWindows() {
		if strings.Contains(string(raw), c.EncryptionKey) {
			t.Error("credentials file contains the encryption key in clear text")
		}
		if strings.Contains(string(raw), c.DevicePassword) {
			t.Error("credentials file contains the device password in clear text")
		}
	}
}

func TestRedactedHidesSecrets(t *testing.T) {
	r := testCreds().Redacted()
	if r.DevicePassword == testCreds().DevicePassword {
		t.Error("device password not redacted")
	}
	if r.EncryptionKey == testCreds().EncryptionKey {
		t.Error("encryption key not redacted")
	}
	if strings.Contains(r.Repository, "@") {
		t.Errorf("repository userinfo survived redaction: %q", r.Repository)
	}
	if r.DeviceID != testCreds().DeviceID || r.Tenant != testCreds().Tenant {
		t.Error("redaction must keep the non-secret fields intact")
	}
}

func TestSanitizeURL(t *testing.T) {
	cases := map[string]string{
		"rest:https://user:pass@backup.softafrique.net/dev-1": "rest:https://backup.softafrique.net/dev-1",
		"rest:https://backup.softafrique.net/dev-1":           "rest:https://backup.softafrique.net/dev-1",
		"s3:https://bucket.s3.amazonaws.com/x?token=abc":      "s3:https://bucket.s3.amazonaws.com/x",
		"https://host/path": "https://host/path",
		"":                  "",
	}
	for in, want := range cases {
		if got := SanitizeURL(in); got != want {
			t.Errorf("SanitizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "credentials.dat"))
	if err := s.Delete(); err != nil {
		t.Fatalf("deleting a missing blob should be a no-op, got %v", err)
	}
	if err := s.Save(testCreds()); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(); err != nil {
		t.Fatal(err)
	}
	if s.Exists() {
		t.Error("blob still present after delete")
	}
}

func TestMatchesDeviceID(t *testing.T) {
	c := testCreds()
	if !c.MatchesDeviceID("dev-01HQ8XK3M4N7") {
		t.Error("expected a match for the enrolled device id")
	}
	if c.MatchesDeviceID("dev-01HQ8XK3M4N") {
		t.Error("a prefix must not match")
	}
	if c.MatchesDeviceID("") {
		t.Error("empty id must not match")
	}
}
