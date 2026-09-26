//go:build windows

package identity

import (
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const currentVersionKey = `SOFTWARE\Microsoft\Windows NT\CurrentVersion`

// collect reads the OS name from the registry.
//
// ProductName cannot be trusted on its own: Windows Server 2016 and 2019 both
// report "Windows Server 2016/2019 Standard" correctly only on patched builds,
// and unpatched or oddly imaged hosts report "Windows 10 ..." while running a
// server SKU. Sending that to the gateway makes a server look like a
// workstation in the dashboard, so the build number is used to correct it.
// DisplayVersion is appended because ProductName does not distinguish
// servicing releases.
func collect() Info {
	info := Info{
		Hostname: hostname(),
		Arch:     arch(),
	}

	k, err := registry.OpenKey(registry.LOCAL_MACHINE, currentVersionKey, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		// A locked-down host, or an unusual WOW64 redirection. Report something
		// rather than nothing: the gateway stores os_caption for display and the
		// agent logs that the precise value is unavailable.
		info.OSCaption = "Windows"
		return info
	}
	defer k.Close()

	product := regString(k, "ProductName")
	info.OSVersion = regString(k, "CurrentBuildNumber")
	display := regString(k, "DisplayVersion")

	caption := product
	if caption == "" {
		caption = "Windows"
	}
	caption = correctServerCaption(caption, info.OSVersion)
	if display != "" && !strings.Contains(caption, display) {
		caption += " " + display
	}
	if info.OSVersion != "" {
		caption += " (build " + info.OSVersion + ")"
	}
	info.OSCaption = caption
	return info
}

// correctServerCaption rewrites the known-bad "Windows 10" ProductName to
// "Windows Server" when the build number proves it is a server SKU. Build
// 14393 is Windows Server 2016; 17763 is Windows Server 2019; 20348 is
// Windows Server 2022. Client builds never reach those.
func correctServerCaption(product, build string) string {
	if !strings.Contains(strings.ToLower(product), "windows 10") {
		return product
	}
	n, err := strconv.Atoi(build)
	if err != nil || n < 14393 {
		return product
	}
	return strings.Replace(product, "Windows 10", "Windows Server", 1)
}

func regString(k registry.Key, name string) string {
	v, _, err := k.GetStringValue(name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "unknown-device"
	}
	return name
}
