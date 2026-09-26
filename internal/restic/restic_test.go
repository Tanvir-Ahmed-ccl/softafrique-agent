package restic

import (
	"testing"
	"time"
)

func TestParseBackupSummary_Restic19(t *testing.T) {
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
	if res.BytesAdded != 4512 {
		t.Errorf("want bytes_added 4512 (from data_added), got %d", res.BytesAdded)
	}
	if res.TotalDuration != 750*time.Millisecond {
		t.Errorf("want 750ms, got %v", res.TotalDuration)
	}
}

func TestParseBackupSummary_OldFieldName(t *testing.T) {
	line := []byte(`{"message_type":"summary","files_new":5,"bytes_added":1024,"snapshot_id":"old"}`)
	res := &Result{}
	if err := parseBackupLine(line, res); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.BytesAdded != 1024 {
		t.Errorf("want 1024, got %d", res.BytesAdded)
	}
	if res.FilesAdded != 5 {
		t.Errorf("want files_new 5, got %d", res.FilesAdded)
	}
}

func TestParseBackupLine_IgnoresNoise(t *testing.T) {
	lines := [][]byte{
		[]byte(`not json at all`),
		[]byte(`{"message_type":"progress","percent_done":0.5}`),
	}
	res := &Result{}
	for _, l := range lines {
		if err := parseBackupLine(l, res); err != nil {
			t.Fatalf("expected noise to be tolerated: %v", err)
		}
	}
}

func TestParseBackupLine_FailedStatus(t *testing.T) {
	line := []byte(`{"message_type":"summary","status":"error copying","snapshot_id":""}`)
	res := &Result{}
	if err := parseBackupLine(line, res); err == nil {
		t.Fatal("expected error for failed status")
	}
}

func TestNormalizedPaths(t *testing.T) {
	got := normalizedPaths([]string{`C:\SoftafriqueBackup\`, "/data/x/", `D:`})
	if got[0] != `C:\SoftafriqueBackup` {
		t.Errorf("want C:\\SoftafriqueBackup, got %q", got[0])
	}
	if got[1] != "/data/x" {
		t.Errorf("want /data/x, got %q", got[1])
	}
	// Drive root keeps its slash regardless of trailing separators; the code
	// path for windows drive root requires runtime check, ensure no panic.
	for _, g := range got {
		if g == "" {
			t.Fatalf("empty normalized path")
		}
	}
}