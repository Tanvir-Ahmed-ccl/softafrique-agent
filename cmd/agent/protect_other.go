//go:build !windows

package main

import (
	"os/user"
	"runtime"
)

// currentUserName returns the account the agent runs as, for the error message
// that tells an operator how to fix a token file's permissions.
func currentUserName() string {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		return "the installing user"
	}
	return u.Username
}

// tokenFileReaders is only consulted on Windows. The build tag keeps the
// platform-specific ACL walk out of every other build.
func tokenFileReaders(path string) ([]string, error) {
	if runtime.GOOS == "windows" {
		panic("unreachable: the Windows build provides its own implementation")
	}
	return nil, nil
}
