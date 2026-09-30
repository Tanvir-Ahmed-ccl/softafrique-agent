// Package pathpolicy decides whether a folder is one this agent is willing to
// back up.
//
// It exists so that the installer and the running agent apply one rule rather
// than two copies of it. The rule is about the volume a folder sits on, and the
// two places it is enforced are a year and a support ticket apart: the MSI checks
// a path a technician typed, the agent checks the same path every hour. When
// that lived in one command's main package, the agent's half had to be a copy,
// and a copy of a policy is a policy that drifts -- the installer refuses a DVD
// drive and the agent quietly snapshots an empty disc tray, or the reverse, and
// neither is visible until a customer notices their data is gone.
//
// The rule, in full:
//
//   - A network share (\\server\share) is refused unless somebody opted in.
//   - An optical drive, removable drive, RAM disk, empty drive letter, or any
//     other volume that is not fixed is refused outright. There is no opt-in,
//     because an ejected disc is not a misconfiguration to be tolerated, it is a
//     backup that stops existing.
//   - A folder on a fixed disk is fine.
//
// Why a network share is the one case with a switch: the service runs as
// LocalSystem, so it reaches a share as the machine account, not as the
// technician who opened it. \\SERVER\CustomerData can work perfectly during
// installation and then fail every hourly backup because the machine account was
// never granted access, with nothing in a backup report to say where. A failure
// that is invisible until someone goes looking is worse than a refused install,
// so allowing the case has to be a decision somebody makes out loud.
//
// Why the rest is refused with no switch at all: on a Server 2019 test machine
// the backup path was pointed at D:, which was the DVD drive. Every check the
// installer could express passed -- D: existed, it was a directory, and with a
// disc in it the directory read fine -- so the install succeeded and then every
// scheduled backup failed with "device is not ready".
package pathpolicy

import (
	"errors"
	"fmt"
	"strings"
)

// Kind names a volume type in words somebody can act on. Returning the raw
// DRIVE_* number would be no use to anyone reading a log or a status file.
type Kind int

const (
	// KindUnknown is a volume whose type could not be worked out. On Windows it
	// is refused, because a type that cannot be read is not evidence of safety.
	KindUnknown Kind = iota
	KindFixed
	KindRemovable
	KindNetwork
	KindOptical
	KindRamDisk
	KindNoRoot
)

func (k Kind) String() string {
	switch k {
	case KindFixed:
		return "a fixed disk"
	case KindRemovable:
		return "a removable drive (USB or memory card)"
	case KindNetwork:
		return "a network share"
	case KindOptical:
		return "an optical drive (CD/DVD)"
	case KindRamDisk:
		return "a RAM disk"
	case KindNoRoot:
		return "a drive letter with no volume mounted"
	default:
		return "an unrecognised volume type"
	}
}

// Opt-in names, for the two callers, which switch or key allows a share.
const (
	// AllowUNCFlag is what a technician types to the installer.
	AllowUNCFlag = "-allow-unc"
	// AllowUNCKey is what is written into config.yaml.
	AllowUNCKey = "allow_unc"
)

// Error is a refusal naming what kind of volume the path turned out to be on.
//
// The kind is carried rather than flattened into the message because "there is
// no such drive" and "that is the wrong kind of drive" need different fixes and
// get different exit codes at install time: the first is a mistyped drive
// letter, the second is a DVD drive where a disk was meant.
type Error struct {
	Path string
	Kind Kind
	// OptIn names the flag or key that would allow this kind of volume, or empty
	// when there is none. It is carried rather than hard-coded into the message
	// so that the installer can say "-allow-unc" and the agent can say
	// "allow_unc: true in config.yaml" for the same refusal, in the vocabulary
	// each of them has.
	OptIn string
}

func (e *Error) Error() string {
	if e.Kind == KindNoRoot {
		return fmt.Sprintf("%s is on drive %s, which is not mounted; check the drive letter",
			e.Path, VolumeName(e.Path))
	}
	msg := fmt.Sprintf("%s is on %s; the folder to protect must be on a fixed disk", e.Path, e.Kind)
	if e.OptIn != "" {
		msg += fmt.Sprintf(" (set %s to allow it deliberately)", e.OptIn)
	}
	return msg
}

// IsUnmounted reports whether err is a refusal because the drive letter has no
// volume mounted, as opposed to the wrong kind of volume.
//
// Exists so callers can stay platform-neutral: they branch on the reason without
// naming a Windows-only constant.
func IsUnmounted(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == KindNoRoot
}

// RequireFixed refuses a path that is not on a fixed disk, or a network share
// that nobody opted in to.
//
// allowUNC is the opt-in, and optIn names it for the message so the refusal tells
// the reader how to proceed in whatever vocabulary they have. Returning an error
// is the only way this package says no: there is no warn-and-continue, because
// every caller here is deciding whether to hand a path to restic, and a path it
// was unsure about is a path it should not hand over.
func RequireFixed(path string, allowUNC bool, optIn string) error {
	// The share test is textual and runs on every platform, including a
	// development machine, because whether a path is a UNC path is a fact about
	// the path and not about the volume table. A test suite that only ran the
	// Windows half of this rule would be a test suite that could not catch the
	// agent refusing a customer's D: drive.
	if IsUNC(path) {
		if allowUNC {
			return nil
		}
		return &Error{Path: path, Kind: KindNetwork, OptIn: optIn}
	}

	kind, decidable := classifyVolume(path)
	if !decidable {
		// Off Windows there is no drive type to read, so the volume part of the
		// rule cannot be applied here. See volume_other.go for why that is allowed
		// through rather than refused.
		return nil
	}
	return decide(path, kind, allowUNC, optIn)
}

// decide is the rule, given what the volume turned out to be.
//
// It is separated from the classification so the table of which volume types are
// allowed can be asserted without asking the machine what drives it happens to
// have. That matters: a test that says "D: is refused" passes on a laptop with
// no DVD drive and fails on a build server where D: is a second fixed disk, so
// it is a test about the machine rather than about the rule.
func decide(path string, kind Kind, allowUNC bool, optIn string) error {
	if kind == KindFixed {
		return nil
	}
	if kind == KindNetwork && allowUNC {
		return nil
	}
	if kind == KindNetwork {
		return &Error{Path: path, Kind: kind, OptIn: optIn}
	}
	// Everything else is refused with no way to allow it, including a type the
	// platform could not name. A volume whose type is unknown is not evidence of
	// safety, and offering a flag that cannot help would only send somebody
	// looking for one.
	return &Error{Path: path, Kind: kind}
}

// IsUNC reports whether a path is a Windows UNC path, \\server\share\folder.
//
// The test is textual rather than a call to the network stack: this decides
// whether the agent is *allowed* to touch a share, and asking the share whether
// it is there would already be the access being gated.
func IsUNC(path string) bool {
	p := strings.TrimSpace(path)
	// A drive letter is local, whatever else is in the path. C:\ and
	// \\server\share both contain a backslash, so testing for separators alone
	// would call every path a share -- and the failure mode of that is the
	// customer's data folder being refused as a policy decision, hours after an
	// install that accepted it.
	if len(p) >= 2 && p[1] == ':' {
		return false
	}
	rest := strings.TrimPrefix(p, `\\`)
	// \folder is a path rooted on the current drive, not a share, and needs a
	// server name before the separator to be one.
	return rest != "" && !strings.HasPrefix(rest, `\`) && strings.Contains(rest, `\`)
}

// VolumeName is the drive or share a path belongs to, for messages.
//
// Split out because filepath.VolumeName does this differently per platform, and
// a message that says the wrong thing about a path is worse than no message.
func VolumeName(path string) string {
	return volumeName(path)
}
