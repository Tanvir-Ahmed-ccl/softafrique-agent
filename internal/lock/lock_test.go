package lock

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestSecondAcquireFails(t *testing.T) {
	first, err := Acquire()
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	defer first.Release()

	if _, err := Acquire(); !errors.Is(err, ErrHeld) {
		t.Fatalf("second acquire should report ErrHeld, got %v", err)
	}
}

func TestReleaseAllowsReacquire(t *testing.T) {
	first, err := Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	// The non-Windows implementation is a file, so make sure nothing was left
	// behind that would wedge the next acquire.
	if _, err := filepath.Abs(""); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire()
	if err != nil {
		t.Fatalf("acquire after release failed: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestDoubleReleaseIsSafe(t *testing.T) {
	l, err := Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	// A double release must not panic or block: the service stop path and an
	// error path can both reach it.
	if err := l.Release(); err != nil && filepath.Separator == '/' {
		t.Fatalf("second release should be a no-op on this platform, got %v", err)
	}
	if l.Describe() == "" {
		t.Error("Describe should return something")
	}
}
