package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Status is the machine-readable state file written after every attempt.
// Tactical RMM and dashboards can tail this file for monitoring.
type Status struct {
	mu sync.Mutex `json:"-"`

	AgentVersion        string    `json:"agent_version"`
	DeviceID            string    `json:"device_id"`
	Repo                string    `json:"repo"`
	LastBackupTime      string    `json:"last_backup_time"`
	LastBackupStatus    string    `json:"last_backup_status"` // success | failed | running
	LastBackupError     string    `json:"last_backup_error"`
	LastSnapshotID      string    `json:"last_snapshot_id"`
	LastDurationSeconds float64   `json:"last_duration_seconds"`
	FilesAdded          uint64    `json:"files_added"`
	FilesChanged        uint64    `json:"files_changed"`
	BytesAdded          uint64    `json:"bytes_added"`
	BackupCount         uint64    `json:"backup_count"`
	NextBackupTime      string    `json:"next_backup_time"`
	LastRunStart        string    `json:"last_run_start"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// Store manages persistence of Status to a JSON file on disk.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore creates a Store writing to path.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// Load reads a previously saved status, returning a zero-value status when
// none exists yet.
func (s *Store) Load() (*Status, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Status{}, nil
		}
		return nil, err
	}
	st := &Status{}
	if err := json.Unmarshal(data, st); err != nil {
		return &Status{}, err
	}
	return st, nil
}

// Save persists st atomically (write temp then rename) so a crash mid-write
// never corrupts the file.
func (s *Store) Save(st *Status) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	st.UpdatedAt = time.Now().UTC()
	st.mu.Lock()
	data, err := json.MarshalIndent(st, "", "  ")
	st.mu.Unlock()
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}