// Package secret stores the device credentials returned by POST /enroll.
//
// The repository password (encryption_key) and the HTTP basic password
// (device_password) are never written to the config file, the status file or
// the log. On Windows they are sealed with DPAPI in machine scope, because the
// service runs as LocalSystem and has no user profile to scope to; on other
// platforms (developer machines only) they are stored in a 0600 file.
package secret

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileName is the blob's name inside the agent data directory.
const FileName = "credentials.dat"

// ErrNotFound means the device is not enrolled: there is no credentials blob.
var ErrNotFound = errors.New("device is not enrolled: no credentials found")

// Credentials is the device's security material plus the non-secret routing
// information the agent needs to reach its repository.
type Credentials struct {
	// DeviceID is the enrollment-assigned device id. It is the ONLY device
	// identifier the agent may use, and the value passed to restic --host.
	DeviceID string `json:"device_id"`

	// DevicePassword is the HTTP basic password for GET /config and
	// POST /status. It is never used as the restic repository password.
	DevicePassword string `json:"device_password"`

	// EncryptionKey is the restic repository password. This is the value that
	// makes the customer's data unrecoverable if lost, which is why it is
	// escrowed server-side.
	EncryptionKey string `json:"encryption_key"`

	// Repository is the restic repository URL exactly as returned by
	// /enroll. It must not contain credentials.
	Repository string `json:"repository"`

	// Tenant is the customer/tenant the device belongs to. Reported in the
	// status file and to the gateway; never a credential.
	Tenant string `json:"tenant"`

	// BackupPath is the folder the gateway wants backed up. It is a default:
	// an explicit include in config.yaml still wins.
	BackupPath string `json:"backup_path"`

	// Server is the gateway API base URL this device enrolled against.
	Server string `json:"server"`

	// EnrolledAt is when enrollment succeeded (RFC3339, UTC).
	EnrolledAt string `json:"enrolled_at"`
}

// ResticPassword returns the repository password to hand to restic.
func (c *Credentials) ResticPassword() string { return c.EncryptionKey }

// BasicAuthUser returns the HTTP basic username (the device id).
func (c *Credentials) BasicAuthUser() string { return c.DeviceID }

// Validate checks that the credential set is complete enough to back up. It
// deliberately does not check the encryption key's entropy, only presence.
func (c *Credentials) Validate() error {
	if c.DeviceID == "" {
		return errors.New("credentials: device_id is empty")
	}
	if c.DevicePassword == "" {
		return errors.New("credentials: device_password is empty")
	}
	if c.EncryptionKey == "" {
		return errors.New("credentials: encryption_key is empty")
	}
	if c.Repository == "" {
		return errors.New("credentials: repository is empty")
	}
	if hasUserInfo(c.Repository) {
		// A userinfo section means the credential ended up in the URL, which is
		// world-readable in a process command line. Reject rather than use it.
		return errors.New("credentials: repository URL must not embed credentials; got " + SanitizeURL(c.Repository))
	}
	return nil
}

// MatchesDeviceID reports whether creds belong to the given device id. Uses a
// constant-time compare so a caller cannot probe the id byte by byte.
func (c *Credentials) MatchesDeviceID(id string) bool {
	return subtle.ConstantTimeCompare([]byte(c.DeviceID), []byte(id)) == 1
}

// Redacted returns a copy safe to log or write to disk: the passwords and the
// encryption key are replaced by a fixed marker.
func (c *Credentials) Redacted() Credentials {
	out := *c
	if out.DevicePassword != "" {
		out.DevicePassword = redactedMarker
	}
	if out.EncryptionKey != "" {
		out.EncryptionKey = redactedMarker
	}
	out.Repository = SanitizeURL(out.Repository)
	return out
}

const redactedMarker = "[redacted]"

// splitAuthority separates a repository URL into the part up to and including
// "://", the authority, and the path+query remainder.
//
// net/url cannot be used here: a restic URL has its own scheme in front of the
// real one ("rest:https://host/path"), so url.Parse puts the whole thing in
// Opaque and never populates User, which would silently hide an embedded
// credential from the check in Validate.
func splitAuthority(raw string) (head, authority, rest string) {
	head, tail := "", raw
	if i := strings.Index(raw, "://"); i >= 0 {
		head, tail = raw[:i+3], raw[i+3:]
	}
	authority, rest = tail, ""
	if i := strings.Index(tail, "/"); i >= 0 {
		authority, rest = tail[:i], tail[i:]
	}
	return head, authority, rest
}

// hasUserInfo reports whether raw carries a user:password@ section.
func hasUserInfo(raw string) bool {
	_, authority, _ := splitAuthority(raw)
	return strings.Contains(authority, "@")
}

// SanitizeURL strips any userinfo and query from a URL so it can be logged.
// rest:https://user:pass@host/path becomes rest:https://host/path.
func SanitizeURL(raw string) string {
	if raw == "" {
		return ""
	}
	head, authority, rest := splitAuthority(raw)
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = authority[at+1:]
	}
	if q := strings.Index(rest, "?"); q >= 0 {
		rest = rest[:q]
	}
	return head + authority + rest
}

// Store persists Credentials with platform-appropriate protection.
type Store struct {
	path string
}

// NewStore returns a Store backed by path.
func NewStore(path string) *Store { return &Store{path: path} }

// Path is the blob's location, for logging.
func (s *Store) Path() string { return s.path }

// Exists reports whether the device has a credentials blob.
func (s *Store) Exists() bool {
	_, err := os.Stat(s.path)
	return err == nil
}

// Save seals c and writes it atomically with owner-only permissions.
func (s *Store) Save(c *Credentials) error {
	if err := c.Validate(); err != nil {
		return err
	}
	plain, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	sealed, err := protect(plain)
	if err != nil {
		return fmt.Errorf("seal credentials: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	// The temp file must never be readable by another user, not even briefly,
	// so it is created with the final permissions rather than chmod-ed after.
	tmp := s.path + ".tmp"
	if err := writePrivate(tmp, sealed); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// os.Rename keeps the temp file's mode, but be explicit: on Windows the
	// ACL set by the installer is what protects this, on POSIX the mode is.
	return os.Chmod(s.path, 0o600)
}

// Load opens and unprotects the stored credentials.
func (s *Store) Load() (*Credentials, error) {
	sealed, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if len(sealed) == 0 {
		return nil, fmt.Errorf("%w: credentials file is empty", ErrNotFound)
	}
	plain, err := unprotect(sealed)
	if err != nil {
		return nil, fmt.Errorf("unseal credentials (the blob may belong to another machine or user): %w", err)
	}
	c := &Credentials{}
	if err := json.Unmarshal(plain, c); err != nil {
		return nil, fmt.Errorf("credentials file is corrupt: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Delete removes the credentials blob. It is used by `agent unenroll` and by
// the 0.1.0 migration; it never contacts the gateway.
func (s *Store) Delete() error {
	err := os.Remove(s.path)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}
