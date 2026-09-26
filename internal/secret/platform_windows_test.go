//go:build windows

package secret

import "runtime"

func isWindows() bool { return runtime.GOOS == "windows" }
