//go:build windows

package main

import "testing"

// Windows treats \\server\share as a volume, so filepath.VolumeName returns the
// whole of \\fileserver\CustomerData and the path is a volume root by the letter
// of the platform's own definition. The volume-type rule therefore has to be
// applied first, or a share is reported as a too-broad folder and the operator is
// never told about the rule that actually refused it -- or has a flag behind it.
//
// This is a Windows-only assertion because it depends on how Windows names
// volumes. It exists because the ordering was wrong and nothing on a macOS
// machine could have shown it.

// A share is refused as a share, whichever side of the share root it is on.
func TestAShareIsRefusedAsAShare(t *testing.T) {
	for _, path := range []string{`\\fileserver\CustomerData`, `\\fileserver\share\CustomerData`} {
		if got := validate([]string{path}); got != 7 {
			t.Errorf("validate(%q) = %d, want 7 (a network share)", path, got)
		}
	}
}

// The refusal happens on the shape of the path, before anything reaches the
// network: on a runner with no file server, \\fileserver cannot resolve, and a
// check that asked the network first would hang or report the wrong thing.
func TestAShareIsRefusedWithoutTouchingTheNetwork(t *testing.T) {
	if got := validate([]string{`\\no-such-server-abc123\share`}); got != 7 {
		t.Errorf("validate = %d, want 7; the decision must not depend on reaching the share", got)
	}
}

// Opting in to a share does not opt in to backing up the whole of it. The share
// root only reaches the root check at all because the volume rule let it past, so
// this is the case that says the two rules are separate.
func TestAShareRootIsStillTooBroadWithTheOptIn(t *testing.T) {
	if got := validate([]string{`-allow-unc`, `\\fileserver\CustomerData`}); got != 5 {
		t.Errorf("validate = %d, want 5 (a share root is still a volume root)", got)
	}
}
