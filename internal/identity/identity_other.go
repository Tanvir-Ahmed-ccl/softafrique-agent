//go:build !windows

package identity

import (
	"os"
	"runtime"
	"strings"
)

// collect describes a non-Windows host. Production devices are Windows-only;
// this exists so the agent can be developed and tested on a developer machine.
func collect() Info {
	info := Info{Arch: arch()}
	if name, err := os.Hostname(); err == nil && name != "" {
		info.Hostname = name
	} else {
		info.Hostname = "unknown-device"
	}
	info.OSCaption = strings.TrimSpace(runtime.GOOS + " " + runtime.GOARCH)
	info.OSVersion = runtime.GOOS
	return info
}
