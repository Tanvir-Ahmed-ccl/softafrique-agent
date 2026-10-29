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
//	0  the path is usable (it may not exist yet; the agent creates it)
//	2  the path is empty
//	3  the path does not exist and neither does the volume it is on
//	4  the path exists but is not a directory
//	5  the path is a drive/share root, a system directory, or the agent's own data directory
//	6  the path cannot be read
//	7  the path is not on a fixed disk
//
// It is deliberately a separate program rather than a DLL so it can be signed,
// so it runs during a managed install as the installing user, and so it has no
// dependency on the agent's Go runtime state.
//
// It checks the folder; it does not create it. A validator with a side effect is
// a validator whose answer depends on whether it ran before, and creating a
// customer folder as the installing user gives it an owner and ACL that the
// service may not be able to use. The agent creates it at backup time, where the
// account doing the work is the account that will use it.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"softafrique-backup-agent/internal/config"
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

	// A fixed disk does not make a system directory a customer folder. The same
	// rule runs in the agent for a path the gateway supplied later.
	if err := pathpolicy.RequireNotProtected(abs, config.DefaultDataDir()); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 5
	}

	// A drive root or share root is refused before the folder is inspected.
	// Backing up a whole volume is expensive and looks like an attack, and
	// -allow-unc opts in to a share, not to all of it.
	if pathpolicy.IsUNC(abs) && isShareRoot(abs) {
		fmt.Fprintf(os.Stderr, "%s is a share root; back up a specific folder on the share instead\n", abs)
		return 5
	}
	if !pathpolicy.IsUNC(abs) && isVolumeRoot(abs) {
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
	case os.IsNotExist(err):
		// A folder that does not exist yet is allowed, but this helper does not
		// create it. The agent does that on its first run, as the service
		// account, so the owner and ACL are right for the process that will use
		// it. The volume still has to be there, or a typo in the drive letter
		// would pass here and fail every backup later.
		if root := volumeRootOf(abs); root != "" {
			if _, rerr := os.Stat(root); rerr != nil {
				fmt.Fprintf(os.Stderr, "%s does not exist and neither does the volume %s it is on\n", abs, root)
				return 3
			}
		}
		fmt.Printf("ok: %s (does not exist yet; the agent will create it)\n", abs)
		return 0
	default:
		// Notably, an unreadable volume reports "device is not ready" or
		// "access is denied" here rather than "does not exist", which is why
		// this is not folded into the IsNotExist branch: it is a real error to
		// report, not a folder waiting to be created.
		fmt.Fprintf(os.Stderr, "cannot read %s: %v\n", abs, err)
		return 6
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

// volumeRootOf is the closest ancestor that must already exist for the path to
// be plausible: the volume root on Windows, the filesystem root elsewhere. Empty
// when it cannot be worked out, in which case the path is accepted and the
// agent's first write reports the real error.
func volumeRootOf(abs string) string {
	if vol := filepath.VolumeName(abs); vol != "" {
		return vol + string(filepath.Separator)
	}
	return string(filepath.Separator)
}

// isShareRoot reports whether a UNC path names only the share, with no folder
// below it. Windows treats \\server\share as a volume root, which is why the
// volume-root message used to call a share root a drive root.
func isShareRoot(path string) bool {
	p := strings.TrimSpace(path)
	p = strings.TrimPrefix(p, `\\`)
	p = strings.Trim(p, `\/`)
	parts := strings.FieldsFunc(p, func(r rune) bool { return r == '\\' || r == '/' })
	return len(parts) == 2
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
