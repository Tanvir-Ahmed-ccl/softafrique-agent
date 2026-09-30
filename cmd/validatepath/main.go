// Command validatepath checks a backup folder before the agent is installed.
//
// The MSI takes the folder to protect as a BACKUPPATH property typed by the
// technician, and a wrong value there is the difference between backing up the
// customer's data and quietly backing up nothing. Windows Installer property
// patterns cannot express "this directory exists, is on a fixed disk, and I can
// read it", so the MSI calls this helper instead and reads the exit code.
//
// Usage:
//
//	validatepath.exe [-allow-unc] <path>
//
// Exit codes, which the MSI maps onto install conditions:
//
//	0  the path is usable, and now exists
//	2  the path is empty
//	3  the path does not exist and neither does the volume it is on
//	4  the path exists but is not a directory
//	5  the path is a drive root, or otherwise too broad to be a customer folder
//	6  the path cannot be read, or does not exist and could not be created
//	7  the path is not on a fixed disk
//
// It is deliberately a separate program rather than a DLL so it can be signed,
// so it runs during a managed install as the installing user, and so it has no
// dependency on the agent's Go runtime state.
//
// Three things it now does that 0.2.0 did not, all asked for after a DVD drive
// was accepted on a test machine and then failed every backup with "device is
// not ready": it **creates** a missing folder, it **refuses** a path that is
// not on a fixed disk, and it **refuses a network share** unless somebody has
// asked for one on purpose with -allow-unc.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"softafrique-backup-agent/internal/pathpolicy"
)

func main() { os.Exit(validate(os.Args[1:])) }

// allowUNCFlag is the one flag, and it is opt-in in the strict sense: nothing
// enables a network share except passing it.
const allowUNCFlag = pathpolicy.AllowUNCFlag

// parseArgs splits the flags from the path.
//
// The rule is deliberately narrow, because BACKUPPATH is typed by hand by a
// technician and a path is not a command line: only a leading -allow-unc is
// taken as a flag, and every other argument is concatenated into the path. That
// keeps a folder name containing a dash or a space working exactly as it did
// before the flag existed.
func parseArgs(args []string) (allowUNC bool, path string) {
	var parts []string
	for _, a := range args {
		if strings.EqualFold(a, allowUNCFlag) {
			allowUNC = true
			continue
		}
		parts = append(parts, a)
	}
	return allowUNC, strings.TrimSpace(strings.Join(parts, " "))
}

func validate(args []string) int {
	allowUNC, raw := parseArgs(args)
	if raw == "" {
		fmt.Fprintln(os.Stderr, "no path given")
		return 2
	}

	path := filepath.Clean(raw)
	if !filepath.IsAbs(path) {
		path, _ = filepath.Abs(path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot resolve %q: %v\n", raw, err)
		return 6
	}

	// What kind of volume this is gets decided before the drive-root check below.
	// Both are refusals, but the volume type is the one that has to be named: a
	// technician who pointed the installer at D:\ on the machine whose D: is a
	// DVD needs to hear "that is a DVD drive", because "back up a specific folder
	// instead" sends them away with a fix that cannot work -- the subfolder they
	// pick is on the same disc.
	//
	// A share root is why this was in the wrong order. Windows treats
	// \\server\share as a volume in its own right, so filepath.VolumeName returns
	// the whole of \\fileserver\CustomerData and the root check claimed it first.
	// The installer was told to back up a specific folder on a path that has no
	// subfolder to pick, and the share rule -- the one an operator needs to hear
	// about, because it is the one with a flag behind it -- was never consulted.
	//
	// The rule itself lives in internal/pathpolicy, which the agent enforces too.
	// A copy here would be a rule that could drift from the one the agent
	// applies at three in the morning, and the drift is what this check is for.
	if err := pathpolicy.RequireFixed(abs, allowUNC, allowUNCFlag); err != nil {
		if pathpolicy.IsUnmounted(err) {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 3
		}
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 7
	}

	// A drive root is checked before anything is created. "Create the folder"
	// must never mean "create a directory at the root of a volume", and backing
	// up a whole drive by accident is expensive and slow enough to look like an
	// attack. A share root gets here only with -allow-unc, and refusing it here is
	// right: opting in to backing up a share is not opting in to backing up all
	// of it.
	if isVolumeRoot(abs) {
		fmt.Fprintf(os.Stderr, "%s is a drive root; back up a specific customer folder instead\n", abs)
		return 5
	}

	info, err := os.Stat(abs)
	switch {
	case err == nil && !info.IsDir():
		fmt.Fprintf(os.Stderr, "%s is a file, not a folder\n", abs)
		return 4
	case err == nil:
		// Falls through to the readability check below.
	case !os.IsNotExist(err):
		// Notably, an unreadable volume reports "device is not ready" or
		// "access is denied" here rather than "does not exist", which is why
		// this is not folded into the IsNotExist branch: it is a real error to
		// report, not a folder waiting to be created.
		fmt.Fprintf(os.Stderr, "cannot read %s: %v\n", abs, err)
		return 6
	default:
		// Not there yet, and that is not a reason to fail the install. The
		// agent protects a configured path, so creating it here is what stops a
		// fresh install for a customer folder that does not exist yet from
		// becoming a machine that reports a failed backup every hour forever.
		//
		// The whole tree is created, not one level: BACKUPPATH is routinely
		// D:\Customer\Data where neither level exists, and refusing that would
		// mean sending a technician back to type a path the installer can
		// perfectly well build.
		//
		// The volume is checked first so a mistyped drive letter is reported as
		// the thing to fix, rather than as a permission problem on a path that
		// could never exist.
		if root := volumeRootOf(abs); root != "" {
			if _, rerr := os.Stat(root); rerr != nil {
				fmt.Fprintf(os.Stderr, "%s does not exist and neither does the volume %s it is on\n", abs, root)
				return 3
			}
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "%s does not exist and could not be created: %v\n", abs, err)
			return 6
		}
		fmt.Printf("created %s\n", abs)
	}

	f, err := os.Open(abs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot read %s: %v\n", abs, err)
		return 6
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err != nil && err.Error() != "EOF" {
		fmt.Fprintf(os.Stderr, "cannot list %s: %v\n", abs, err)
		return 6
	}

	fmt.Printf("ok: %s\n", abs)
	return 0
}

// volumeRootOf is the closest existing ancestor that must be present for a
// create to have any chance: the volume root on Windows, the filesystem root
// elsewhere. Empty when it cannot be worked out, in which case the create is
// simply attempted and its own error reported.
func volumeRootOf(abs string) string {
	if vol := filepath.VolumeName(abs); vol != "" {
		return vol + string(filepath.Separator)
	}
	return string(filepath.Separator)
}

// isVolumeRoot reports whether path is a mount point such as C:\ or /.
func isVolumeRoot(path string) bool {
	clean := filepath.Clean(path)
	if runtime.GOOS != "windows" {
		return clean == "/"
	}
	vol := filepath.VolumeName(clean)
	rest := strings.TrimPrefix(clean, vol)
	return strings.Trim(rest, `\/`) == ""
}
