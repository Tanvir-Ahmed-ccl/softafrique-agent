// Package api is the client for the Softafrique backup gateway.
//
//	Base URL: https://backup.softafrique.net/api/v1
//
//	POST /enroll   no auth, one-time token
//	GET  /config   HTTP basic (device_id / device_password)
//	POST /status   HTTP basic (device_id / device_password)
//	GET  /health   no auth
//
// The gateway is append-only: this package exposes no method that could delete
// or expire a snapshot, and the agent never calls restic's forget or prune.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultBaseURL is the production gateway.
const DefaultBaseURL = "https://backup.softafrique.net/api/v1"

// EnrollRequest is the POST /enroll body.
type EnrollRequest struct {
	// Token is the single-use enrollment token issued by the dashboard.
	Token string `json:"token"`
	// Hostname is the sanitized machine name.
	Hostname string `json:"hostname"`
	// OSCaption is the OS name, e.g. "Windows Server 2019 Standard".
	OSCaption string `json:"os_caption"`
	// BackupPath is the folder the customer wants protected.
	BackupPath string `json:"backup_path"`
	// AgentVersion is the build enrolling.
	AgentVersion string `json:"agent_version"`
}

// EnrollResponse is the POST /enroll reply. Every field is required; a reply
// missing any of them is rejected rather than half-applied, because a device
// with a repository but no key cannot back up and a device with a key but no
// repository cannot find it.
type EnrollResponse struct {
	Tenant         string `json:"tenant"`
	DeviceID       string `json:"device_id"`
	Repository     string `json:"repository"`
	DevicePassword string `json:"device_password"`
	EncryptionKey  string `json:"encryption_key"`
	BackupPath     string `json:"backup_path"`
}

// Validate rejects an incomplete enrollment reply.
func (r *EnrollResponse) Validate() error {
	var missing []string
	if r.DeviceID == "" {
		missing = append(missing, "device_id")
	}
	if r.Repository == "" {
		missing = append(missing, "repository")
	}
	if r.DevicePassword == "" {
		missing = append(missing, "device_password")
	}
	if r.EncryptionKey == "" {
		missing = append(missing, "encryption_key")
	}
	if len(missing) > 0 {
		return fmt.Errorf("gateway /enroll reply is missing required field(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// RedactedForLog returns a copy with the two secrets masked. The enrollment
// reply is logged once at enrollment time so support can see which tenant and
// which repository a device landed in, and that log must not become a place
// where keys accumulate.
func (r *EnrollResponse) RedactedForLog() EnrollResponse {
	out := *r
	if out.DevicePassword != "" {
		out.DevicePassword = "[redacted]"
	}
	if out.EncryptionKey != "" {
		out.EncryptionKey = "[redacted]"
	}
	return out
}

// DeviceState is the gateway's view of whether a device may back up.
type DeviceState string

const (
	// StateActive means the device is enrolled and may back up. This is the
	// only value that permits a backup run.
	StateActive DeviceState = "active"
	// StateSuspended means the gateway has withdrawn the device. Any value the
	// gateway returns that is not "active" is treated as suspended: an
	// unrecognised state must not silently keep a decommissioned device
	// writing to the repository.
	StateSuspended DeviceState = "suspended"
)

// RemoteConfig is the GET /config reply.
type RemoteConfig struct {
	DeviceID   string      `json:"device_id"`
	BackupPath string      `json:"backup_path"`
	Status     DeviceState `json:"status"`

	// ScheduleInterval and RetryInterval accept either a Go duration string
	// ("1h", "30m") or a plain number of seconds. The contract does not pin the
	// wire format, so both are accepted rather than failing to parse a working
	// configuration.
	ScheduleInterval Duration `json:"schedule_interval"`
	RetryInterval    Duration `json:"retry_interval"`
}

// IsActive reports whether the gateway permits backups.
func (c *RemoteConfig) IsActive() bool {
	return c != nil && c.Status == StateActive
}

// SuspendReason explains a non-active status for the status file and the log.
func (c *RemoteConfig) SuspendReason() string {
	if c == nil {
		return "no configuration received yet"
	}
	if c.Status == "" {
		return "gateway returned no status field; treating the device as suspended"
	}
	return fmt.Sprintf("gateway reports device status %q", string(c.Status))
}

// StatusReport is the POST /status body.
//
// The fields are exactly the gateway's contract. Nothing extra is sent: the
// richer monitoring view (tenant, OS, backup paths, repository usage, last
// attempt versus last success) lives in status.json for Tactical RMM, because
// adding undeclared fields to this contract would be guessing at a server-side
// schema. If the gateway wants more, it is a change on Mohsin's side.
//
// LastBackupStatus is only ever "success" or "failed": a report is sent after
// an attempt has finished, never while one is running.
type StatusReport struct {
	LastBackupStatus    string  `json:"last_backup_status"`
	LastSnapshotID      string  `json:"last_snapshot_id"`
	FilesAdded          uint64  `json:"files_added"`
	FilesChanged        uint64  `json:"files_changed"`
	BytesAdded          uint64  `json:"bytes_added"`
	LastDurationSeconds float64 `json:"last_duration_seconds"`
	LastBackupError     string  `json:"last_backup_error"`
	AgentVersion        string  `json:"agent_version"`
	OSCaption           string  `json:"os_caption"`
}

// Duration is a time.Duration that unmarshals from either "1h"/"30m" or a
// number of seconds.
type Duration time.Duration

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		*d = 0
		return nil
	}
	// A JSON string: try Go duration syntax first, then bare seconds.
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			*d = 0
			return nil
		}
		parsed, err := time.ParseDuration(s)
		if err != nil {
			// "3600" as a string still means seconds.
			var secs float64
			if jsonErr := json.Unmarshal([]byte(s), &secs); jsonErr != nil {
				return fmt.Errorf("cannot parse interval %q: %w", s, err)
			}
			*d = Duration(time.Duration(secs * float64(time.Second)))
			return nil
		}
		*d = Duration(parsed)
		return nil
	}
	var secs float64
	if err := json.Unmarshal(b, &secs); err != nil {
		return fmt.Errorf("cannot parse interval %s: %w", trimmed, err)
	}
	*d = Duration(time.Duration(secs * float64(time.Second)))
	return nil
}

// MarshalJSON implements json.Marshaler, emitting Go duration syntax.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// Sentinels for the failure modes the agent must react to differently.
var (
	// ErrTokenInvalid means the enrollment token is bad, expired or already
	// used. A new token must be issued; retrying is pointless.
	ErrTokenInvalid = errors.New("enrollment token is invalid, expired or already used")
	// ErrAlreadyEnrolled means the gateway has a record for this token/device.
	ErrAlreadyEnrolled = errors.New("device is already enrolled with the gateway")
	// ErrBadRequest means the gateway refused the payload, e.g. a hostname it
	// will not accept.
	ErrBadRequest = errors.New("gateway rejected the request")
	// ErrUnauthorized means the device credentials were refused, i.e. the
	// enrollment is no longer valid. The device needs to re-enroll.
	ErrUnauthorized = errors.New("gateway rejected the device credentials")
	// ErrRevoked means the gateway understood the credentials but has withdrawn
	// the device, so backups must stop.
	ErrRevoked = errors.New("gateway has revoked this device")
	// ErrUnavailable means the gateway could not be reached or answered with a
	// server error. This is never fatal to a backup run: the agent keeps the
	// last known configuration and tries again.
	ErrUnavailable = errors.New("gateway is unavailable")
)

// Error is a failed gateway call.
type Error struct {
	Op         string // "enroll", "config", "status", "health"
	Method     string
	Path       string
	StatusCode int
	// Body is the response body, truncated. It is safe to log: the gateway does
	// not echo credentials, and any userinfo in a URL is stripped before the
	// URL is formatted into the message.
	Body string
	// Err is the sentinel this error matches, or a transport error.
	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: ", e.Op, e.Path)
	if e.StatusCode != 0 {
		fmt.Fprintf(&b, "HTTP %d", e.StatusCode)
	} else {
		b.WriteString("no response")
	}
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	if e.Body != "" {
		fmt.Fprintf(&b, " (%s)", e.Body)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// Is lets callers branch on the sentinels with errors.Is.
func (e *Error) Is(target error) bool { return e.Err == target }

// Temporary reports whether retrying later could plausibly succeed. A bad
// token or revoked device is not temporary; a transport failure or a 5xx is.
func (e *Error) Temporary() bool {
	return errors.Is(e, ErrUnavailable)
}

const maxBodyBytes = 64 << 10
