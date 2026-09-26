//go:build !windows

package secret

func isWindows() bool { return false }
