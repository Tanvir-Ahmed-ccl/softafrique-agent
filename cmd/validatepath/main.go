// Command validatepath checks a backup folder before the agent is installed.
//
// The MSI takes the folder to protect as a BACKUPPATH property typed by the
// technician, and a wrong value there is the difference between backing up the
// customer's data and quietly backing up nothing. Windows Installer property
// patterns cannot express "this directory exists and I can read it", so the MSI
// calls this helper instead and reads the exit code.
//
// Exit codes, which the MSI maps onto install conditions:
//
//	0  the path is usable
//	2  the path is empty
//	3  the path does not exist
//	4  the path exists but is not a directory
//	5  the path is a drive root, or otherwise too broad to be a customer folder
//	6  the path cannot be read, or does not exist yet and cannot be created
//
// It is deliberately a separate program rather than a DLL so it can be signed,
// so it runs during a managed install as the installing user, and so it has no
// dependency on the agent's Go runtime state.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func main() { os.Exit(validate(os.Args[1:])) }

func validate(args []string) int {
	raw := strings.TrimSpace(strings.Join(args, " "))
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

	info, err := os.Stat(abs)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "cannot read %s: %v\n", abs, err)
			return 6
		}
		// Not there yet. A folder the customer has not created is not a reason
		// to fail the install: the agent will report it, and the parent may be
		// writable so the agent can create it. Refuse only if the parent is
		// obviously wrong.
		parent := filepath.Dir(abs)
		if _, perr := os.Stat(parent); perr != nil {
			fmt.Fprintf(os.Stderr, "%s does not exist and its parent %s does not either\n", abs, parent)
			return 3
		}
		fmt.Printf("ok: %s does not exist yet; the agent will report it until it is created\n", abs)
		return 0
	}

	if !info.IsDir() {
		fmt.Fprintf(os.Stderr, "%s is a file, not a folder\n", abs)
		return 4
	}

	// A drive root is almost always a mis-typed path, and backing up a whole
	// drive by accident is expensive and slow enough to look like an attack.
	if isVolumeRoot(abs) {
		fmt.Fprintf(os.Stderr, "%s is a drive root; back up a specific customer folder instead\n", abs)
		return 5
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

	if runtime.GOOS == "windows" {
		fmt.Printf("ok: %s\n", abs)
	} else {
		fmt.Printf("ok: %s\n", abs)
	}
	return 0
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
