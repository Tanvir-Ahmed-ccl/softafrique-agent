// Command agent is the Softafrique backup agent: a Windows service that backs
// up a customer's folder to an append-only restic repository, and a small CLI
// around it for enrollment, checks and restores.
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
	"softafrique-backup-agent/internal/lock"
	"softafrique-backup-agent/internal/restic"
	"softafrique-backup-agent/internal/secret"
	"softafrique-backup-agent/internal/service"
	"softafrique-backup-agent/internal/status"
)

// Version is overridden at build time via -ldflags.
var Version = "0.2.0-dev"

// EnrollTokenEnv lets the RMM hand over a token without it appearing in a
// command line, where any local process could read it out of the process table.
const EnrollTokenEnv = "SOFTAFRIQUE_ENROLL_TOKEN"

// Exit codes, so an RMM task result says what went wrong without parsing text.
const (
	exitOK = 0
	// exitError is a generic failure.
	exitError = 1
	// exitUsage is a bad command line.
	exitUsage = 2
	// exitNotEnrolled means the device has no credentials yet.
	exitNotEnrolled = 3
	// exitActionRequired means the gateway has withdrawn the device and a human
	// has to intervene.
	exitActionRequired = 4
	// exitNotHealthy means a check ran but found a problem, e.g. no successful
	// backup inside the expected window.
	exitNotHealthy = 5
)

func main() { os.Exit(run(os.Args[1:])) }

// runtimeEnv is the resolved environment for a command.
type runtimeEnv struct {
	cfg     *config.Config
	log     *slog.Logger
	store   *status.Store
	console bool
}

func run(args []string) int {
	// The Windows service control manager starts the exe with no arguments at
	// all, so "no arguments" means service mode rather than a usage error.
	if len(args) == 0 {
		if err := cmdRun(nil, true); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return exitError
		}
		return exitOK
	}
	cmd, rest := args[0], args[1:]

	// These do not need config, a logger or a lock, so they are dispatched
	// before anything is opened.
	switch cmd {
	case "version", "--version", "-v":
		fmt.Printf("Softafrique Backup Agent %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
		return exitOK
	case "help", "--help", "-h":
		usage(os.Stdout)
		return exitOK
	case "render-config":
		// Dispatched here rather than with the rest, because it runs during
		// install, before config.yaml exists. Loading the environment first
		// would fail on the very file this command is there to produce.
		if err := cmdRenderConfig(rest); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return exitError
		}
		return exitOK
	}

	var err error
	switch cmd {
	case "enroll":
		err = cmdEnroll(rest)
	case "unenroll":
		err = cmdUnenroll(rest)
	case "reimport":
		err = cmdReimport(rest)
	case "run":
		err = cmdRun(rest, false)
	case "service":
		err = cmdRun(rest, true)
	case "install":
		err = cmdInstall(rest)
	case "uninstall":
		err = cmdUninstall(rest)
	case "backup":
		err = cmdBackup(rest)
	case "selftest", "doctor":
		err = cmdSelfTest(rest)
	case "snapshots":
		err = cmdSnapshots(rest)
	case "status":
		err = cmdStatus(rest)
	case "restore":
		err = cmdRestore(rest)
	default:
		usage(os.Stderr)
		return exitUsage
	}

	if err == nil {
		return exitOK
	}
	if errors.Is(err, flag.ErrHelp) {
		return exitUsage
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	return exitCodeFor(err)
}

// exitCodeFor maps an error onto the RMM-facing exit code.
func exitCodeFor(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, agent.ErrNotEnrolled), errors.Is(err, secret.ErrNotFound):
		return exitNotEnrolled
	case errors.Is(err, context.Canceled):
		return exitOK
	case isActionRequired(err):
		return exitActionRequired
	default:
		return exitError
	}
}

// actionRequiredError is returned when a human has to do something.
type actionRequiredError struct{ msg string }

func (e *actionRequiredError) Error() string { return e.msg }

func actionRequired(format string, args ...any) error {
	return &actionRequiredError{msg: fmt.Sprintf(format, args...)}
}

func isActionRequired(err error) bool {
	var target *actionRequiredError
	return errors.As(err, &target)
}

// openEnv builds the config, logger and status store for a command.
func openEnv(args []string, console bool, extra func(fs *flag.FlagSet) error) (*runtimeEnv, error) {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if extra != nil {
		if err := extra(fs); err != nil {
			return nil, err
		}
	}
	cfgPath := fs.String("config", config.DefaultConfigPath(), "path to config.yaml")
	repo := fs.String("repo", "", "override the repository URL (unenrolled devices only)")
	passwordFile := fs.String("password-file", "", "restic password file (unenrolled devices only)")
	var include *string
	if fs.Lookup("include") == nil {
		include = fs.String("include", "", "override the backup folders (comma separated)")
	}
	deviceID := fs.String("device-id", "", "device id (unenrolled devices only)")
	verbose := fs.Bool("verbose", false, "log at debug level")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	overrides := map[string]string{}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if given["repo"] {
		overrides["repo"] = *repo
	}
	if given["password-file"] {
		overrides["password-file"] = *passwordFile
	}
	if include != nil && given["include"] {
		overrides["include"] = *include
	}
	if given["device-id"] {
		overrides["device-id"] = *deviceID
	}
	if given["verbose"] && *verbose {
		overrides["verbose"] = "true"
	}

	cfg, err := config.Load(*cfgPath, overrides)
	if err != nil {
		return nil, fmt.Errorf("could not load %s: %w", *cfgPath, err)
	}
	log, err := newLogger(cfg, console || cfg.Verbose)
	if err != nil {
		return nil, err
	}
	return &runtimeEnv{cfg: cfg, log: log, store: status.NewStore(cfg.StatusFile), console: console}, nil
}

// newLogger writes to the configured log file and, in console mode, to stderr.
//
// Timestamps are always UTC. A backup agent that logs local time produces logs
// that cannot be lined up with gateway records across a DST boundary, which is
// precisely when somebody is trying to work out what happened at 2am.
func newLogger(cfg *config.Config, console bool) (*slog.Logger, error) {
	var writers []io.Writer
	if cfg.LogFile != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.LogFile), 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("could not open the log file: %w", err)
		}
		writers = append(writers, f)
	}
	if console {
		writers = append(writers, os.Stderr)
	}
	if len(writers) == 0 {
		writers = append(writers, io.Discard)
	}
	level := slog.LevelInfo
	if cfg.Verbose {
		level = slog.LevelDebug
	}
	handler := slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{Level: level})
	logger := slog.New(&utcHandler{inner: handler})
	return logger, nil
}

// utcHandler forces UTC on every record's timestamp.
type utcHandler struct{ inner slog.Handler }

func (h *utcHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *utcHandler) Handle(ctx context.Context, r slog.Record) error {
	utc := slog.NewRecord(r.Time.UTC(), r.Level, r.Message, r.PC)
	r = utc
	return h.inner.Handle(ctx, r)
}

func (h *utcHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &utcHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *utcHandler) WithGroup(name string) slog.Handler {
	return &utcHandler{inner: h.inner.WithGroup(name)}
}

func signalCtx() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func newAgent(env *runtimeEnv) *agent.Agent {
	a := agent.New(env.cfg, env.store, env.log)
	a.Version = Version
	return a
}

// acquireLock takes the single-instance lock.
//
// A second `agent run` or a scheduled `agent backup` while the service is
// already looping would put two restic processes on the same repository under
// the same --host, and both would rewrite status.json.
func acquireLock() (lock.Lock, error) {
	l, err := lock.Acquire()
	if err != nil {
		if errors.Is(err, lock.ErrHeld) {
			return nil, actionRequired("another agent instance is already running; " +
				"start the service or the console agent, not both")
		}
		return nil, err
	}
	return l, nil
}

func cmdEnroll(args []string) error {
	var token, tokenFile string
	env, err := openEnv(args, true, func(fs *flag.FlagSet) error {
		fs.StringVar(&token, "token", "", "one-time enrollment token (prefer "+EnrollTokenEnv+" or -token-file)")
		fs.StringVar(&tokenFile, "token-file", "", "read the token from this file")
		return nil
	})
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()

	resolved, fromFlag, err := resolveToken(token, tokenFile)
	if err != nil {
		return err
	}
	// The environment is checked first, so remember whether the file was even
	// the source. Deleting a file we did not read would be a surprise and a
	// small data-loss bug.
	tokenCameFromEnv := strings.TrimSpace(os.Getenv(EnrollTokenEnv)) != ""
	if fromFlag {
		env.log.Warn("the enrollment token was passed on the command line, where any local process can read it " +
			"from the process table; use " + EnrollTokenEnv + " or -token-file instead")
	}

	creds, err := newAgent(env).Enroll(ctx, resolved)
	if err == nil && tokenFile != "" && !tokenCameFromEnv {
		// A one-time token has just been consumed, so the file holding it is
		// now a dead secret sitting on disk. The MSI and the RMM script both
		// rely on this to clean up after themselves.
		if rmErr := os.Remove(tokenFile); rmErr != nil {
			env.log.Warn("enrollment succeeded but the token file could not be deleted; "+
				"delete it by hand, it is a consumed one-time secret", "path", tokenFile, "err", rmErr)
		} else {
			env.log.Info("removed the consumed enrollment token file", "path", tokenFile)
		}
	}
	if err != nil {
		return err
	}
	fmt.Printf("enrolled as %s\n", creds.DeviceID)
	fmt.Printf("  tenant      %s\n", creds.Tenant)
	fmt.Printf("  repository  %s\n", creds.Repository)
	fmt.Printf("  protects    %s\n", creds.BackupPath)
	fmt.Printf("  key storage %s\n           %s\n", env.cfg.SecretStore().Path(), env.cfg.SecretStore().Protection())
	return nil
}

// resolveToken picks the token from, in order of preference, the environment,
// a file, or the flag.
// resolveToken picks the enrollment token from, in order, the environment
// variable, the token file, then -token.
//
// The environment wins so an RMM task can set the token in the process
// environment and still pass -token-file for the fallback path. The bool result
// is "came from the flag", which only -token can be: a secret typed on a command
// line is visible in the process list, so the file and the environment are
// preferred and the flag is a last resort for an operator at a console.
func resolveToken(flagToken, tokenFile string) (value string, fromFlag bool, err error) {
	if v := strings.TrimSpace(os.Getenv(EnrollTokenEnv)); v != "" {
		return v, false, nil
	}
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", false, fmt.Errorf("could not read the token file: %w", err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", false, fmt.Errorf("the token file %s is empty", tokenFile)
		}
		if err := checkTokenFileProtection(tokenFile); err != nil {
			return "", false, err
		}
		return v, false, nil
	}
	if v := strings.TrimSpace(flagToken); v != "" {
		return v, true, nil
	}
	return "", false, fmt.Errorf("no enrollment token: set %s, or pass -token-file, or -token", EnrollTokenEnv)
}

// cmdRenderConfig writes config.yaml from the installer's template, filling in
// the paths that are only known at install time.
//
// The MSI used to do this with the Util extension's ConfigurableTextFile, which
// WiX 4 removed and WiX 5 never replaced, so the substitution now happens here.
// Keeping it in the agent means the result is parsed and validated before it is
// installed, rather than by the service on its first boot.
func cmdRenderConfig(args []string) error {
	fs := flag.NewFlagSet("render-config", flag.ContinueOnError)
	template := fs.String("template", "", "config template to read (required)")
	out := fs.String("out", "", "path to write (required)")
	backupPath := fs.String("backup-path", "", "folder to protect (required)")
	dataDir := fs.String("data-dir", "", "agent state directory (default the standard one)")
	statusFile := fs.String("status-file", "", "status.json path (default inside data-dir)")
	logFile := fs.String("log-file", "", "agent log path (default inside data-dir)")
	force := fs.Bool("force", false, "overwrite an existing file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *template == "" || *out == "" {
		fs.Usage()
		return errors.New("render-config needs -template and -out")
	}

	wrote, err := config.RenderToFile(*template, *out, config.RenderOptions{
		BackupPath: *backupPath,
		DataDir:    *dataDir,
		StatusFile: *statusFile,
		LogFile:    *logFile,
	}, *force)
	if err != nil {
		return err
	}
	if !wrote {
		// This is the NeverOverwrite case: a repair or an upgrade must not
		// discard a config a technician edited on site.
		fmt.Printf("kept existing %s\n", *out)
		return nil
	}
	fmt.Printf("wrote %s\n", *out)
	return nil
}

func cmdUnenroll(args []string) error {
	var assumeYes bool
	env, err := openEnv(args, true, func(fs *flag.FlagSet) error {
		fs.BoolVar(&assumeYes, "yes", false, "do not ask for confirmation")
		return nil
	})
	if err != nil {
		return err
	}
	if !env.cfg.SecretStore().Exists() {
		fmt.Println("this device has no stored credentials; nothing to do")
		return nil
	}
	if !assumeYes {
		return actionRequired("this deletes the encryption key from this machine, after which a restore from " +
			"this device is impossible without the escrowed key. Re-run with -yes if that is intended")
	}
	if err := newAgent(env).Unenroll(); err != nil {
		return err
	}
	fmt.Println("credentials deleted from this device; the repository and its snapshots are untouched")
	return nil
}

// cmdReimport rebuilds the credentials blob from a key file.
//
// This is the recovery path for a machine that was reimaged: the gateway still
// has the device record and the escrowed key, but the DPAPI blob went with the
// disk. It is deliberately not part of the RMM scripts, because it needs a
// human with the key in hand.
func cmdReimport(args []string) error {
	var keyFile, devicePasswordFile string
	env, err := openEnv(args, true, func(fs *flag.FlagSet) error {
		fs.StringVar(&keyFile, "key-file", "", "file containing the escrowed encryption key")
		fs.StringVar(&devicePasswordFile, "device-password-file", "", "file containing the device password")
		return nil
	})
	if err != nil {
		return err
	}
	if keyFile == "" || devicePasswordFile == "" {
		return fmt.Errorf("both -key-file and -device-password-file are required")
	}
	if env.cfg.DeviceID == "" {
		return fmt.Errorf("set device_id in the config, or pass -device-id: it must match the gateway's record")
	}
	key, err := readSecret(keyFile)
	if err != nil {
		return err
	}
	pass, err := readSecret(devicePasswordFile)
	if err != nil {
		return err
	}
	// The backup path is whatever the operator already has working. An empty one
	// used to index off the end of an empty slice and panic the CLI, which is a
	// poor way to learn that a config is incomplete.
	paths := env.cfg.BackupPaths("")
	if len(paths) == 0 {
		return fmt.Errorf("no backup path is configured: set include in the config, " +
			"or let the agent take it from the gateway at the next successful config fetch")
	}
	// The repository comes from the config, not the gateway: re-import exists
	// for the case where the gateway cannot be reached, so asking it would
	// defeat the purpose. A repository that still carries the placeholder after
	// substitution would silently write to the wrong place, so it is named.
	repo := strings.ReplaceAll(env.cfg.Repo, "{device_id}", env.cfg.DeviceID)
	if strings.Contains(repo, "{device_id}") {
		return fmt.Errorf("the configured repo still contains a device id placeholder: %q", env.cfg.Repo)
	}
	creds := &secret.Credentials{
		DeviceID:       env.cfg.DeviceID,
		DevicePassword: pass,
		EncryptionKey:  key,
		Repository:     repo,
		Server:         env.cfg.Server,
		BackupPath:     paths[0],
		EnrolledAt:     status.Now(),
	}
	if err := creds.Validate(); err != nil {
		return err
	}
	if err := env.cfg.SecretStore().Save(creds); err != nil {
		return err
	}
	fmt.Printf("credentials re-imported for %s\n", creds.DeviceID)
	fmt.Printf("  repository  %s\n", creds.Repository)
	fmt.Printf("  backup path %s\n", creds.BackupPath)
	fmt.Printf("\nconfirm the repository and path above before relying on this machine: " +
		"re-import takes them from the config, not from the gateway, so a mistake here is silent\n")
	return nil
}

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("could not read %s: %w", path, err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return v, nil
}

func cmdRun(args []string, asService bool) error {
	env, err := openEnv(args, !asService, nil)
	if err != nil {
		return err
	}
	a := newAgent(env)

	run := func(ctx context.Context) error {
		l, err := acquireLock()
		if err != nil {
			return err
		}
		defer l.Release()
		env.log.Info("single-instance lock acquired", "lock", l.Describe())
		env.log.Info("starting the backup loop", "version", Version, "mode", modeName(asService))
		return a.RunScheduled(ctx)
	}

	if asService {
		return service.RunService(run, env.log)
	}
	ctx, stop := signalCtx()
	defer stop()
	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	env.log.Info("backup loop stopped")
	return nil
}

func modeName(asService bool) string {
	if asService {
		return "windows service"
	}
	return "console"
}

func cmdInstall(args []string) error {
	env, err := openEnv(args, true, nil)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// The MSI normally creates the service; this is the manual fallback.
	if err := service.InstallService(exe); err != nil {
		return fmt.Errorf("%w (if the service already exists, that is fine: the MSI installed it)", err)
	}
	fmt.Printf("service %s installed; start it with: sc start %s\n", service.Name, service.Name)
	_ = env
	return nil
}

func cmdUninstall(args []string) error {
	if _, err := openEnv(args, true, nil); err != nil {
		return err
	}
	if err := service.UninstallService(); err != nil {
		return err
	}
	fmt.Printf("service %s removed; the credentials in %s were left alone\n",
		service.Name, config.DefaultDataDir())
	return nil
}

func cmdBackup(args []string) error {
	env, err := openEnv(args, true, nil)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()

	l, err := acquireLock()
	if err != nil {
		return err
	}
	defer l.Release()

	a := newAgent(env)
	outcome, err := a.BackupNow(ctx)
	if err != nil {
		printOutcome(env, outcome)
		return err
	}
	printOutcome(env, outcome)
	if suspended, why := a.Suspension(); suspended {
		return actionRequired("backups are paused: %s", why)
	}
	return nil
}

func printOutcome(env *runtimeEnv, outcome status.Outcome) {
	if outcome.Err != nil {
		fmt.Fprintf(os.Stderr, "backup failed after %s: %v\n",
			outcome.Elapsed.Round(time.Second), outcome.Err)
		return
	}
	fmt.Printf("snapshot %s in %s\n", shortID(outcome.SnapshotID), outcome.Elapsed.Round(time.Second))
	fmt.Printf("  files  +%d new, %d changed\n", outcome.FilesAdded, outcome.FilesChanged)
	fmt.Printf("  bytes  %s\n", humanBytes(outcome.BytesAdded))
	fmt.Printf("  restic worked for %s; the rest was waiting on the network\n",
		outcome.ResticDuration.Round(time.Second))
	_ = env
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

func cmdSelfTest(args []string) error {
	var verbose bool
	env, err := openEnv(args, true, func(fs *flag.FlagSet) error {
		fs.BoolVar(&verbose, "v", false, "print where everything lives")
		return nil
	})
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()

	a := newAgent(env)
	if verbose {
		// The credential store is not inside state_dir: it is machine wide so
		// the service can read it as SYSTEM and so a re-import can find it after
		// a rebuild. That makes it invisible from the config, and "where is the
		// key on this machine" is the first question in almost every support
		// call. The one-line pass/fail below cannot answer it, so -v can.
		store := env.cfg.SecretStore()
		fmt.Printf("agent         %s\n", Version)
		fmt.Printf("data dir      %s\n", env.cfg.DataDir)
		fmt.Printf("credentials   %s (%s)\n", store.Path(), store.Protection())
		fmt.Printf("gateway       %s\n", env.cfg.Server)
		if creds, err := store.Load(); err == nil {
			fmt.Printf("device        %s (tenant %s)\n", creds.DeviceID, creds.Tenant)
			fmt.Printf("repository    %s\n", restic.RedactedRepository)
			fmt.Printf("backup path   %s\n", creds.BackupPath)
		} else {
			fmt.Println("device        not enrolled")
		}
	}
	if err := a.SelfTest(ctx); err != nil {
		return err
	}
	fmt.Println("repository reachable, key accepted, gateway reachable")
	return nil
}

func cmdSnapshots(args []string) error {
	env, err := openEnv(args, true, nil)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()

	snap, err := newAgent(env).LatestSnapshot(ctx)
	if err != nil {
		return err
	}
	if snap == nil {
		fmt.Println("no snapshots yet")
		return nil
	}
	fmt.Printf("snapshot  %s\n", snap.ID)
	fmt.Printf("  taken   %s\n", snap.Time.UTC().Format(time.RFC3339))
	fmt.Printf("  host    %s\n", snap.Host)
	fmt.Printf("  paths   %s\n", strings.Join(snap.Paths, ", "))
	_ = env
	return nil
}

func cmdStatus(args []string) error {
	var short bool
	env, err := openEnv(args, true, func(fs *flag.FlagSet) error {
		fs.BoolVar(&short, "short", false, "print a one-line summary for monitoring scripts")
		return nil
	})
	if err != nil {
		return err
	}
	st, err := env.store.Load()
	if err != nil {
		return err
	}
	if short {
		fmt.Println(shortStatus(st))
		return nil
	}
	out, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

// shortStatus is the line a monitoring script matches on. It is deliberately
// plain: no colour, no locale-dependent formatting, one line, no secrets.
func shortStatus(st *status.Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "enrolled=%t", st.Enrolled)
	fmt.Fprintf(&b, " device=%s", orDash(st.DeviceID))
	fmt.Fprintf(&b, " tenant=%s", orDash(st.Tenant))
	fmt.Fprintf(&b, " last_attempt=%s", orDash(st.LastAttemptStatus))
	fmt.Fprintf(&b, " last_success=%s", orDash(st.LastSuccess))
	fmt.Fprintf(&b, " consecutive_failures=%d", st.ConsecutiveFailures)
	fmt.Fprintf(&b, " gateway=%s", orDash(st.ServerStatus))
	if st.RepoStats != nil {
		fmt.Fprintf(&b, " repo_bytes=%d", st.RepoStats.RepoBytes)
		fmt.Fprintf(&b, " snapshots=%d", st.RepoStats.SnapshotCount)
	}
	if st.ActionRequired != "" {
		fmt.Fprintf(&b, " action=%q", st.ActionRequired)
	}
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func defaultRestoreTarget() string {
	if runtime.GOOS == "windows" {
		return filepath.Join("C:", "SoftafriqueRestore")
	}
	return filepath.Join(".", "restore_output")
}

func cmdRestore(args []string) error {
	var snapshot, target string
	var includes multiFlag
	env, err := openEnv(args, true, func(fs *flag.FlagSet) error {
		fs.StringVar(&snapshot, "snapshot", "latest", "snapshot id, or \"latest\"")
		fs.StringVar(&target, "target", defaultRestoreTarget(), "folder to restore into")
		fs.Var(&includes, "include", "path pattern to restore (repeatable)")
		return nil
	})
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()

	l, err := acquireLock()
	if err != nil {
		return err
	}
	defer l.Release()

	if err := newAgent(env).Restore(ctx, snapshot, target, includes); err != nil {
		return err
	}
	fmt.Printf("restored %s into %s\n", snapshot, target)
	_ = env
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `Softafrique Backup Agent %s - encrypted backups for Softafrique customers.

Usage:
  agent <command> [flags]

Setup:
  enroll      Exchange a one-time token for this device's credentials.
  unenroll    Delete the credentials from this device (local only).
  reimport    Rebuild the credentials from an escrowed key after a reimage.

Running:
  run         Run the scheduled backup loop in the foreground.
  service     Windows service entry point, started by the SCM.
  install     Install the Windows service by hand (the MSI normally does this).
  uninstall   Remove the Windows service.

Operations:
  backup      Run one backup now and report what happened.
  selftest    Check the repository, the key and the gateway.
  snapshots   Show the latest snapshot.
  status      Print status.json, or -short for one monitoring line.
  restore     Restore a snapshot into a folder.
  render-config  Write config.yaml from the installer template (used by the MSI).
  version     Print the version.

Common flags:
  -config <path>        Config file (default %s)
  -include <folders>    Override the folders to back up (comma separated)
  -verbose              Log at debug level

enroll flags:
  -token <t>            One-time token. Prefer the %s environment
                        variable or -token-file: a token on the command line is
                        visible to every local process.
  -token-file <path>    Read the token from a file.

restore flags:
  -snapshot <id|latest> Snapshot to restore (default latest)
  -target <dir>         Destination folder
  -include <pattern>    Restrict the restore to matching paths (repeatable)

Exit codes:
  0 ok   1 error   2 bad usage   3 not enrolled   4 action required   5 unhealthy

Examples:
  agent enroll
  agent run -config %s
  agent backup
  agent restore -snapshot latest -target C:\\SoftafriqueRestore
`, Version, config.DefaultConfigPath(), EnrollTokenEnv, config.DefaultConfigPath())
}

// checkTokenFileProtection refuses a token file other accounts can read.
//
// The token is a one-time secret: anyone who reads it can enroll a machine of
// their own as this customer. On Windows a file's mode bits are synthesised by
// Go and say nothing about the ACL, so the check has to be platform specific;
// everywhere else the mode is the honest answer.
func checkTokenFileProtection(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("could not inspect the token file: %w", err)
	}
	if runtime.GOOS != "windows" {
		if info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("%s is readable by other users; run: icacls %s /inheritance:r /grant:r \"%s:F\" \"SYSTEM:F\"",
				path, path, currentUserName())
		}
		return nil
	}
	allowed, err := tokenFileReaders(path)
	if err != nil {
		// Do not block enrollment on an audit we could not perform, but say so.
		fmt.Fprintf(os.Stderr, "warning: could not verify who can read %s: %v\n", path, err)
		return nil
	}
	for _, who := range allowed {
		if !isPrivilegedTokenReader(who) {
			return fmt.Errorf("%s is readable by %q; run: icacls %s /inheritance:r /grant:r \"%s:F\" \"SYSTEM:F\"",
				path, who, path, currentUserName())
		}
	}
	return nil
}

// isPrivilegedTokenReader reports whether an account allowed to read a token
// file is one that is expected to run the installer: SYSTEM, the Administrators
// group, or the user running the install.
//
// Accounts arrive from the ACL in several spellings: "SYSTEM",
// "NT AUTHORITY\SYSTEM", "BUILTIN\Administrators" and a SID when the name could
// not be resolved. All of them name the same trustees, so the account name is
// compared on the part after the last separator, with the well-known SIDs as a
// fallback.
func isPrivilegedTokenReader(who string) bool {
	w := strings.ToUpper(strings.TrimSpace(who))
	if w == "" {
		return true
	}
	switch w {
	case "S-1-5-18", "S-1-5-32-544", "S-1-5-32-545":
		return true
	}
	name := w
	if i := strings.LastIndex(name, `\`); i >= 0 {
		name = name[i+1:]
	}
	if name == "SYSTEM" || name == "LOCAL SYSTEM" {
		return true
	}
	// "Administrators" and "Administrators (elevated)" are the same trustee;
	// an installer that grants the group also grants the built-in Users group in
	// some templates, so match the group and not any word containing it.
	if name == "ADMINISTRATORS" || name == "ADMINISTRATOR" {
		return true
	}
	return w == strings.ToUpper(currentUserName()) || name == strings.ToUpper(currentUserName())
}
