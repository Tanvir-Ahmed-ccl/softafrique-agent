package restic

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testRestic(t *testing.T, mutate func(*Options)) *Restic {
	t.Helper()
	opts := Options{
		Binary:     "restic",
		Repository: "rest:https://backup.softafrique.net/dev-01HQ8XK3M4N7",
		Host:       "dev-01HQ8XK3M4N7",
		Password:   "e3b1f0a95c2d47e6b8a10f34c9d2e7b56",
		Logger:     discardLogger(),
	}
	if mutate != nil {
		mutate(&opts)
	}
	return New(opts)
}

func TestArgsCarryHostAndNoPassword(t *testing.T) {
	r := testRestic(t, nil)
	args := r.args("backup")
	joined := strings.Join(args, " ")

	if !containsPair(args, "--host", "dev-01HQ8XK3M4N7") {
		t.Errorf("--host must carry the enrollment device id, got %v", args)
	}
	if !containsPair(args, "-r", "rest:https://backup.softafrique.net/dev-01HQ8XK3M4N7") {
		t.Errorf("-r must carry the repository, got %v", args)
	}
	// The password must reach restic through the environment, never argv, where
	// every local user can read it with Win32_Process.
	if strings.Contains(joined, r.opts.Password) {
		t.Errorf("the repository password leaked into the command line: %v", args)
	}
	if strings.Contains(joined, "--password") {
		t.Errorf("no password flag expected when PasswordFile is unset: %v", args)
	}
}

func TestArgsUsePasswordFileWhenSet(t *testing.T) {
	r := testRestic(t, func(o *Options) { o.PasswordFile = `C:\ProgramData\pw` })
	args := r.args("snapshots")
	if !containsPair(args, "--password-file", `C:\ProgramData\pw`) {
		t.Errorf("expected --password-file, got %v", args)
	}
}

func TestArgsNeverContainRetentionCommands(t *testing.T) {
	// The gateway is append-only and retention is the operator's job. This test
	// is the code-level form of the guarantee Mohsin checked in the gateway
	// logs: there is no argument list this package can build that deletes or
	// expires anything.
	r := testRestic(t, nil)
	invocations := [][]string{
		r.args("backup"),
		r.args("snapshots"),
		r.args("--json", "snapshots", "--latest", "1"),
		r.args("--json", "stats", "--mode", ModeRawData),
		r.args("restore", "latest", "--target", "C:\\out"),
	}
	forbidden := map[string]bool{
		"forget": true, "prune": true, "unlock": true, "copy": true,
		"tag": true, "check": false, // check is read-only and allowed
	}
	for _, args := range invocations {
		for _, arg := range args {
			if bad, ok := forbidden[arg]; ok && bad {
				t.Errorf("restic %s must never be invoked by this agent", arg)
			}
		}
	}
}

// The gateway creates the repository server-side at enrollment, and enrollment
// fails if its restic init does not. Mohsin confirmed that, and asked for
// auto_init to stay off permanently.
//
// "Off" was a default once, which is not the same thing as permanent: a
// hand-edited config could turn it back on, and on a machine nobody is watching
// a key that can be set is a key that gets set. So the option is gone and the
// init path with it. This test is what makes that a property of the code rather
// than a claim in a document -- a future change that reopens the door has to
// delete this test to get through, and deleting it is a visible act.
func TestNoInitCodePathExists(t *testing.T) {
	if _, ok := reflect.TypeOf(Options{}).FieldByName("AllowInit"); ok {
		t.Error("restic.Options has an AllowInit field; the agent must not be able to create a repository")
	}

	// Read the package's own source. Checking the fields alone would not notice
	// somebody calling r.args("init") from a function that takes no option, which
	// is exactly the shape a careless fix would take.
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, name := range entries {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			code := line
			if idx := strings.Index(code, "//"); idx >= 0 {
				code = code[:idx]
			}
			if strings.Contains(code, `"init"`) {
				t.Errorf("%s:%d builds a restic init invocation: %s", name, i+1, strings.TrimSpace(line))
				found++
			}
		}
	}
	if found == 0 && len(entries) == 0 {
		t.Error("no source files were read; this test is not checking anything")
	}
}

func TestParseBackupSummaryRestic19(t *testing.T) {
	line := []byte(`{"message_type":"summary","files_new":0,"files_changed":2,"files_unmodified":1,"dirs_new":0,"dirs_changed":8,"data_added":4512,"total_duration":0.75,"snapshot_id":"abc123"}`)
	res := &Result{}
	if err := parseBackupLine(line, res); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.SnapshotID != "abc123" {
		t.Errorf("want snapshot abc123, got %q", res.SnapshotID)
	}
	if res.FilesChanged != 2 {
		t.Errorf("want files_changed 2, got %d", res.FilesChanged)
	}
	// restic 0.19 reports data_added; 0.1.0 read bytes_added only and would have
	// reported 0 here.
	if res.BytesAdded != 4512 {
		t.Errorf("want bytes_added 4512 (from data_added), got %d", res.BytesAdded)
	}
	if res.ResticDuration != 750*time.Millisecond {
		t.Errorf("want 750ms, got %v", res.ResticDuration)
	}
}

func TestParseBackupSummaryOldFieldName(t *testing.T) {
	line := []byte(`{"message_type":"summary","files_new":5,"bytes_added":1024,"snapshot_id":"old"}`)
	res := &Result{}
	if err := parseBackupLine(line, res); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.BytesAdded != 1024 || res.FilesAdded != 5 {
		t.Errorf("got bytes=%d files=%d", res.BytesAdded, res.FilesAdded)
	}
}

func TestParseBackupLineIgnoresNoise(t *testing.T) {
	lines := [][]byte{
		[]byte(`not json at all`),
		[]byte(`{"message_type":"progress","percent_done":0.5}`),
		[]byte(`{"message_type":"verbose_status","seconds_elapsed":3}`),
	}
	res := &Result{}
	for _, l := range lines {
		if err := parseBackupLine(l, res); err != nil {
			t.Fatalf("expected noise to be tolerated: %v", err)
		}
	}
	if res.SnapshotID != "" {
		t.Error("noise must not populate the result")
	}
}

func TestParseBackupLineFailedStatus(t *testing.T) {
	line := []byte(`{"message_type":"summary","status":"error copying","snapshot_id":""}`)
	res := &Result{}
	if err := parseBackupLine(line, res); err == nil {
		t.Fatal("expected error for failed status")
	}
}

func TestReasonIsActionable(t *testing.T) {
	cases := map[int]string{
		ExitRepoMissing:          "does not exist",
		ExitRepoConfigUnreadable: "password is wrong",
		ExitRepoLocked:           "lock",
		ExitSourceError:          "source files",
		1:                        "restic reported an error",
	}
	for code, want := range cases {
		if got := Reason(code); !strings.Contains(got, want) {
			t.Errorf("Reason(%d) = %q, want it to mention %q", code, got, want)
		}
	}
}

func TestRepoErrorIncludesResticOutput(t *testing.T) {
	sink := newStderrSink(discardLogger(), "")
	sink.Write([]byte("Fatal: wrong password or no key found\n"))
	err := &RepoError{Op: "backup", Code: ExitRepoConfigUnreadable, Reason: Reason(ExitRepoConfigUnreadable), Detail: sink.Tail()}
	if !strings.Contains(err.Error(), "wrong password") {
		t.Errorf("restic's own message should reach the error, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "exit 11") {
		t.Errorf("exit code should be in the message, got %q", err.Error())
	}
}

func TestStderrSinkLogsCompleteLinesOnce(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sink := newStderrSink(log, "")

	// One message arriving in two writes, as it would from a pipe.
	sink.Write([]byte("Fatal: unable to open "))
	sink.Write([]byte("config file\n"))
	sink.Write([]byte("}\n"))

	got := strings.Count(buf.String(), "restic")
	if got != 2 {
		t.Errorf("expected 2 log lines, got %d: %s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "unable to open config file") {
		t.Errorf("split line was not reassembled: %s", buf.String())
	}
	if !strings.Contains(sink.Tail(), "Fatal: unable to open config file") {
		t.Errorf("tail should carry restic's last words, got %q", sink.Tail())
	}
}

func TestStderrSinkTailIsBounded(t *testing.T) {
	sink := newStderrSink(discardLogger(), "")
	for i := 0; i < 1000; i++ {
		sink.Write([]byte("a line of restic output that is fairly long indeed\n"))
	}
	if got := len(sink.Tail()); got > 8<<10 {
		t.Errorf("tail grew to %d bytes; it must stay bounded", got)
	}
}

func TestExitCodeOfNonExitError(t *testing.T) {
	// A missing binary is not an exit code, and must not be mistaken for one.
	// This used to matter to EnsureRepo, which used to run init when restic was
	// absent; EnsureRepo no longer has an init path at all, which
	// TestNoInitCodePathExists pins.
	if _, ok := ExitCode(errors.New("exec: \"restic\": executable file not found in $PATH")); ok {
		t.Error("a spawn failure must not report an exit code")
	}
	if _, ok := ExitCode(context.Canceled); ok {
		t.Error("a cancellation must not report an exit code")
	}
	if code, ok := ExitCode(nil); !ok || code != 0 {
		t.Error("no error should be exit code 0")
	}
}

func TestIsRepoMissingRequiresTheRealCode(t *testing.T) {
	if IsRepoMissing(errors.New("some other failure")) {
		t.Error("a plain error is not a missing repository")
	}
	if IsRepoMissing(nil) {
		t.Error("nil is not a missing repository")
	}
}

func TestStatsRejectsUnknownMode(t *testing.T) {
	r := testRestic(t, nil)
	if _, err := r.Stats(context.Background(), "banana"); err == nil {
		t.Fatal("expected an unknown mode to be rejected before restic is run")
	}
}

func TestNormalizedPaths(t *testing.T) {
	got := normalizedPaths([]string{`C:\SoftafriqueBackup\`, "/data/x/", `D:`, ""})
	if got[0] != `C:\SoftafriqueBackup` {
		t.Errorf("want C:\\SoftafriqueBackup, got %q", got[0])
	}
	if got[1] != "/data/x" {
		t.Errorf("want /data/x, got %q", got[1])
	}
	if len(got) != 3 {
		t.Errorf("empty paths should be dropped, got %v", got)
	}
	for _, g := range got {
		if g == "" {
			t.Fatal("empty normalized path")
		}
	}
}

func containsPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// restic echoes the repository URL in its own errors, and those errors reach the
// log, the console and the RMM task output. The status report omits the
// repository and the documentation promises the same for the log, so the URL is
// replaced where restic output enters the program.
func TestStderrSinkRedactsTheRepository(t *testing.T) {
	const repo = "rest:https://backup.example.net/dev-42"
	sink := newStderrSink(discardLogger(), repo)

	lines := []string{
		"Fatal: unable to open config file: unexpected HTTP response (401)\n",
		"repository " + repo + " is locked\n",
		"repository " + repo + "/ is locked\n",
		"see " + repo + " for details\n",
	}
	for _, line := range lines {
		if _, err := sink.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	tail := sink.Tail()
	if strings.Contains(tail, repo) {
		t.Errorf("the repository URL survived redaction: %q", tail)
	}
	if strings.Contains(tail, "dev-42") {
		t.Errorf("the device id survived redaction: %q", tail)
	}
	if !strings.Contains(tail, RedactedRepository) {
		t.Errorf("expected a redaction marker in %q", tail)
	}
	// The part that diagnoses the failure has to survive, or the redaction has
	// destroyed the only useful part of the message.
	if !strings.Contains(tail, "401") {
		t.Errorf("the diagnostic detail was lost: %q", tail)
	}
}

// A sink built without a repository, as the unit tests do, must pass text through
// untouched rather than substituting an empty string for everything.
func TestStderrSinkWithoutRepositoryIsUntouched(t *testing.T) {
	sink := newStderrSink(discardLogger(), "")
	const line = "Fatal: the repository rest:https://x/y is locked\n"
	if _, err := sink.Write([]byte(line)); err != nil {
		t.Fatal(err)
	}
	// Tail trims the trailing newline, so compare the text that matters.
	if got := strings.TrimSpace(sink.Tail()); got != strings.TrimSpace(line) {
		t.Errorf("text was altered with no repository configured: %q", got)
	}
}
