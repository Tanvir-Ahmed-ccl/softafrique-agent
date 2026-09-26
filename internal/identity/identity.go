// Package identity collects the facts about this machine that the gateway and
// the monitoring dashboard need: hostname, OS caption and architecture.
package identity

import (
	"runtime"
	"strings"
)

// Info describes the host. OSCaption is the field sent as os_caption to
// /enroll and /status.
type Info struct {
	// Hostname is the machine name as Windows reports it.
	Hostname string `json:"hostname"`

	// OSCaption is a human-readable OS name, e.g.
	// "Windows Server 2019 Standard 10.0.17763".
	OSCaption string `json:"os_caption"`

	// OSVersion is the build number, e.g. "17763".
	OSVersion string `json:"os_version"`

	// Arch is the agent's architecture, e.g. "amd64".
	Arch string `json:"arch"`
}

// Collect gathers the host identity. It never fails: a field that cannot be
// read is left empty rather than blocking enrollment, because the gateway
// needs a usable value and the agent logs what is missing.
func Collect() Info {
	return collect()
}

// EnrollHostname returns the hostname to send to /enroll, sanitized to
// something the gateway will accept (the endpoint rejects anything else with
// 400, and there is no point spending a single-use token on a request that is
// going to be refused).
func EnrollHostname() string {
	return SanitizeHostname(Collect().Hostname)
}

// SanitizeHostname lowercases a machine name and reduces it to the
// lowercase-alphanumeric-and-dash form the gateway accepts, capped at 63
// characters. An empty result becomes "unknown-device" so the field is never
// empty.
func SanitizeHostname(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.' || r == ' ':
			b.WriteRune('-')
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	if out == "" {
		return "unknown-device"
	}
	return out
}

// arch reports the agent's own architecture.
func arch() string {
	if runtime.GOARCH == "" {
		return "unknown"
	}
	return runtime.GOARCH
}
