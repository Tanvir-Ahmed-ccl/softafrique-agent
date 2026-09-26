//go:build windows

package lock

import (
	"errors"
	"sync"

	"golang.org/x/sys/windows"
)

// mutexName is in the Global namespace so the lock spans terminal sessions:
// a service running as LocalSystem in session 0 and an operator's elevated
// shell are different sessions, and a per-session mutex would not collide.
const mutexName = `Global\SoftafriqueBackupAgent.SingleInstance`

type winLock struct {
	handle windows.Handle
	once   sync.Once
}

func acquire() (Lock, error) {
	name, err := windows.UTF16PtrFromString(mutexName)
	if err != nil {
		return nil, err
	}
	// initialOwner=true so this instance actually owns the mutex rather than
	// merely opening it.
	h, err := windows.CreateMutex(nil, true, name)
	if err != nil {
		// A pre-existing mutex is not a failure: CreateMutex still returns a
		// valid handle and reports ERROR_ALREADY_EXISTS. Close that handle or
		// this process would keep the mutex alive after returning, and the
		// agent would deadlock itself on the next start.
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			_ = windows.CloseHandle(h)
			return nil, ErrHeld
		}
		return nil, err
	}
	return &winLock{handle: h}, nil
}

func (l *winLock) Release() error {
	var err error
	l.once.Do(func() {
		_ = windows.ReleaseMutex(l.handle)
		err = windows.CloseHandle(l.handle)
	})
	return err
}

func (l *winLock) Describe() string { return "mutex " + mutexName }
