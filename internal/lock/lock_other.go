//go:build !windows

package lock

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// lockFile is the non-Windows implementation. It exists so the agent can be
// developed and tested on a developer machine; production devices use the named
// mutex in lock_windows.go.
const lockFile = "agent.lock"

type fileLock struct {
	path string
}

type lockRecord struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

func acquire() (Lock, error) {
	dir := os.TempDir()
	path := filepath.Join(dir, "softafrique-backup-agent."+lockFile)
	record := lockRecord{PID: os.Getpid(), StartedAt: time.Now().UTC().Format(time.RFC3339)}
	body, _ := json.Marshal(record)

	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.Write(body)
			_ = f.Close()
			return &fileLock{path: path}, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		// A lock file left behind by a killed process must not wedge the agent
		// forever, so take it over when the recorded pid is gone.
		if !processAlive(path) {
			_ = os.Remove(path)
			continue
		}
		return nil, ErrHeld
	}
	return nil, fmt.Errorf("could not acquire lock file %s", path)
}

func processAlive(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var rec lockRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return false
	}
	if rec.PID <= 0 {
		return false
	}
	p, err := os.FindProcess(rec.PID)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func (l *fileLock) Release() error {
	err := os.Remove(l.path)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

func (l *fileLock) Describe() string {
	pid := "?"
	if data, err := os.ReadFile(l.path); err == nil {
		var rec lockRecord
		if json.Unmarshal(data, &rec) == nil {
			pid = strconv.Itoa(rec.PID)
		}
	}
	return strings.TrimSpace("lockfile " + l.path + " pid " + pid)
}
