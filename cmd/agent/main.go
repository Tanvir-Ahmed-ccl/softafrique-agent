package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"softafrique-backup-agent/internal/agent"
	"softafrique-backup-agent/internal/config"
	"softafrique-backup-agent/internal/service"
	"softafrique-backup-agent/internal/status"
)

// Version is overridden at build time via -ldflags.
var Version = "0.1.0-dev"

type agentRT struct {
	cfg   *config.Config
	log   *slog.Logger
	store *status.Store
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	cmd, rest := args[0], args[1:]

	var err error
	switch cmd {
	case "version", "--version", "-v":
		fmt.Printf("Softafrique Backup Agent %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
		return 0

	case "help", "--help", "-h":
		usage(os.Stdout)
		return 0

	case "run":
		err = cmdConsole(rest)

	case "service":
		err = cmdService(rest)

	case "install":
		err = cmdInstall(rest)

	case "uninstall":
		err = cmdUninstall(rest)

	case "backup":
		err = cmdQuick(rest, cmd)

	case "selftest":
		err = cmdQuick(rest, cmd)

	case "snapshots":
		err = cmdQuick(rest, cmd)

	case "status":
		err = cmdStatus(rest)

	case "restore":
		err = cmdRestore(rest)

	default:
		usage(os.Stderr)
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

// openRuntime creates a FlagSet, registers the command's specific flags (via
// extra) plus the common flags, parses args, then loads config/logger/store.
// extraFlags may register flags whose values are read through closures after
// parsing, and may also be nil.
func openRuntime(args []string, console bool, extraFlags func(fs *flag.FlagSet) error) (*agentRT, error) {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if extraFlags != nil {
		if err := extraFlags(fs); err != nil {
			return nil, err
		}
	}
	cfgPath := fs.String("config", defaultConfigPath(), "path to config.yaml")
	repo := fs.String("repo", "", "override repo URL (supports {device_id})")
	passwordFile := fs.String("password-file", "", "override restic password file")
	// -include is the path override for most commands but is already claimed by
	// `restore` as its own honest-to-god include pattern; don't double register.
	var include *string
	if fs.Lookup("include") == nil {
		include = fs.String("include", "", "override backup folders (comma separated)")
	}
	deviceID := fs.String("device-id", "", "override device id")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	overrides := map[string]string{}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["repo"] {
		overrides["repo"] = *repo
	}
	if set["password-file"] {
		overrides["password-file"] = *passwordFile
	}
	if include != nil && set["include"] {
		overrides["include"] = *include
	}
	if set["device-id"] {
		overrides["device-id"] = *deviceID
	}

	cfg, err := config.Load(*cfgPath, overrides)
	if err != nil {
		return nil, fmt.Errorf("load config %s: %w", *cfgPath, err)
	}

	log, err := newLogger(cfg.LogFile, console || cfg.Verbose)
	if err != nil {
		return nil, err
	}
	store := status.NewStore(cfg.StatusFile)
	return &agentRT{cfg: cfg, log: log, store: store}, nil
}

func defaultConfigPath() string { return config.DefaultConfigPath() }

// newLogger writes to the configured file, and also to stderr in console mode.
func newLogger(path string, console bool) (*slog.Logger, error) {
	writers := []io.Writer{}
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("open log file: %w", err)
		}
		writers = append(writers, f)
	}
	if console {
		writers = append(writers, os.Stderr)
	}
	if len(writers) == 0 {
		writers = append(writers, io.Discard)
	}
	return slog.New(slog.NewTextHandler(io.MultiWriter(writers...), nil)), nil
}

func signalCtx() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// newAgent builds the agent and stamps its version.
func newAgent(rt *agentRT) *agent.Agent {
	a := agent.New(rt.cfg, rt.store, rt.log)
	a.Version = Version
	return a
}

func cmdConsole(args []string) error {
	rt, err := openRuntime(args, true, nil)
	if err != nil {
		return err
	}
	rt.log.Info("starting console mode", "version", Version, "repo", rt.cfg.Repo)
	rt.log.Info("the backup server runs append-only so a compromised PC cannot delete its backups")

	ctx, stop := signalCtx()
	defer stop()

	a := newAgent(rt)
	if err := a.RunScheduled(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func cmdService(args []string) error {
	rt, err := openRuntime(args, false, nil)
	if err != nil {
		return err
	}
	a := newAgent(rt)
	return service.RunService(a.RunScheduled, rt.log)
}

func cmdInstall(args []string) error {
	if _, err := openRuntime(args, false, nil); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := service.InstallService(exe); err != nil {
		return err
	}
	fmt.Printf("Service %s installed. Start it from the Services console or:\n  sc start %s\n", service.Name, service.Name)
	return nil
}

func cmdUninstall(args []string) error {
	if err := service.UninstallService(); err != nil {
		return err
	}
	fmt.Printf("Service %s uninstalled.\n", service.Name)
	return nil
}

func cmdQuick(args []string, cmd string) error {
	rt, err := openRuntime(args, true, nil)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()

	a := newAgent(rt)
	switch cmd {
	case "backup":
		id, err := a.RunBackup(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("backup complete, snapshot %s\n", id)
	case "selftest":
		if err := a.SelfTest(ctx); err != nil {
			return err
		}
		fmt.Println("repo reachable and credentials OK")
	case "snapshots":
		snap, err := a.LatestSnapshot(ctx)
		if err != nil {
			return err
		}
		if snap == nil {
			fmt.Println("no snapshots yet")
			return nil
		}
		fmt.Printf("latest snapshot %s at %s, paths: %s\n", snap.ID, snap.Time.Format(time.RFC3339), strings.Join(snap.Paths, ", "))
	}
	return nil
}

func cmdStatus(args []string) error {
	rt, err := openRuntime(args, false, nil)
	if err != nil {
		return err
	}
	st, err := rt.store.Load()
	if err != nil {
		return err
	}
	if st.DeviceID == "" {
		st.DeviceID = rt.cfg.DeviceID
		st.Repo = rt.cfg.Repo
	}
	out, _ := json.MarshalIndent(st, "", "  ")
	fmt.Println(string(out))
	return nil
}

func defaultRestoreTarget() string {
	if runtime.GOOS == "windows" {
		return filepath.Join("C:", "SoftafriqueRestore")
	}
	return "restore_output"
}

func cmdRestore(args []string) error {
	var snapshot, target string
	var includes multiFlag
	rt, err := openRuntime(args, true, func(fs *flag.FlagSet) error {
		fs.StringVar(&snapshot, "snapshot", "latest", "snapshot id to restore, or \"latest\"")
		fs.StringVar(&target, "target", defaultRestoreTarget(), "folder to restore into")
		fs.Var(&includes, "include", "path pattern to restore (repeatable)")
		return nil
	})
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()

	a := newAgent(rt)
	if err := a.Restore(ctx, snapshot, target, includes); err != nil {
		return err
	}
	fmt.Printf("restored snapshot %s into %s\n", snapshot, target)
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `Softafrique Backup Agent — secure restic-based backups for Softafrique customers.

Usage:
  agent <command> [flags]

Commands:
  run         Run the scheduled backup loop in the foreground (console).
  service     Windows Service entry point (started by the SCM; usable for
              manual debugging too).
  install     Install the Windows service (Windows only).
  uninstall   Remove the Windows service (Windows only).
  backup      Run a single backup immediately and print the snapshot id.
  selftest    Verify the repo is reachable and credentials work.
  snapshots   Show the latest snapshot.
  status      Print the JSON status file.
  restore     Restore a snapshot into a target folder.
  version     Print the version.

Common flags:
  -config <path>          Path to config.yaml (default: %s)
  -repo <url>             Override repo URL (supports {device_id})
  -password-file <path>   Override restic password file
  -include <dirs>         Override backup folders (comma separated)
  -device-id <id>         Override device id

Restore flags:
  -snapshot <id|latest>   Snapshot to restore (default: latest)
  -target <dir>           Destination folder
  -include <pattern>      Restrict restore to matching paths (repeatable)

Example:
  agent run -config C:\ProgramData\SoftafriqueBackupAgent\config.yaml
`, defaultConfigPath())
}