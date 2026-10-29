//go:build !windows

package config

// dataDirACLWarning is Windows-only: POSIX file modes are the boundary here,
// and DefaultDataDir is already private to the user that created it.
func dataDirACLWarning(string) string { return "" }
