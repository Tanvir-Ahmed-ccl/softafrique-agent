// Package lock prevents two agent instances from running at once.
//
// This matters because an operator (or a monitoring script) can start a
// one-shot `agent backup` while the Windows service is already looping. Two
// restic processes would then write snapshots to the same repository under the
// same --host, and both would race on status.json. restic's own repository lock
// only serialises them; it does not stop the double schedule.
package lock

import "errors"

// ErrHeld means another instance already holds the lock.
var ErrHeld = errors.New("another agent instance is already running")

// Lock is a held single-instance lock. Release must be called exactly once.
type Lock interface {
	// Release gives up the lock. It is safe to call on a nil Lock.
	Release() error
	// Describe returns a short human-readable description for logs.
	Describe() string
}

// Acquire takes the single-instance lock. It returns ErrHeld when another
// instance has it.
func Acquire() (Lock, error) { return acquire() }
