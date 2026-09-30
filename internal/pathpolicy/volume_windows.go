//go:build windows

package pathpolicy

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// classifyVolume reports what kind of volume backs path, and whether this
// platform can tell at all. It always can, here.
//
// The drive letter is taken from the path rather than from the process's current
// directory, so a relative path is judged by where it actually points.
func classifyVolume(path string) (Kind, bool) {
	root := volumeRoot(path)
	if root == "" {
		return KindUnknown, true
	}
	utf16, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return KindUnknown, true
	}
	switch windows.GetDriveType(utf16) {
	case windows.DRIVE_FIXED:
		return KindFixed, true
	case windows.DRIVE_REMOVABLE:
		return KindRemovable, true
	case windows.DRIVE_REMOTE:
		// GetDriveType on a share's root reports DRIVE_REMOTE, which is how a
		// network share is caught without a second code path.
		return KindNetwork, true
	case windows.DRIVE_CDROM:
		return KindOptical, true
	case windows.DRIVE_RAMDISK:
		return KindRamDisk, true
	case windows.DRIVE_NO_ROOT_DIR:
		return KindNoRoot, true
	default:
		return KindUnknown, true
	}
}

// volumeRoot is the string GetDriveType wants: "C:\" for a drive letter, or the
// share root for a UNC path.
func volumeRoot(path string) string {
	clean := strings.TrimSpace(path)
	if clean == "" {
		return ""
	}
	if IsUNC(clean) {
		// \\server\share\folder -> \\server\share
		parts := strings.Split(strings.TrimPrefix(clean, `\\`), `\`)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return clean
		}
		return `\\` + parts[0] + `\` + parts[1]
	}
	vol := filepath.VolumeName(clean)
	if vol == "" {
		return clean
	}
	return vol + `\`
}

func volumeName(path string) string { return filepath.VolumeName(path) }
