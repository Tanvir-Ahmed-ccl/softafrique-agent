package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"softafrique-backup-agent/internal/api"
)

// remoteCache remembers the last successful GET /config.
//
// The point is the reboot case: a device that comes up with no network still
// knows which folder to back up and how often, so it does not sit idle for a
// full schedule interval waiting to be told. It holds nothing secret: the
// device id is already in the credentials blob, and the backup path is a
// folder name the customer gave us.
type remoteCache struct {
	path string
}

type cachedRemote struct {
	DeviceID         string       `json:"device_id"`
	BackupPath       string       `json:"backup_path"`
	Status           string       `json:"status"`
	ScheduleInterval api.Duration `json:"schedule_interval"`
	RetryInterval    api.Duration `json:"retry_interval"`
	FetchedAt        string       `json:"fetched_at"`
}

func newRemoteCache(path string) *remoteCache { return &remoteCache{path: path} }

// Load returns the cached configuration, or nil when there is none or it is
// unreadable. A corrupt cache is not an error worth stopping a backup for: the
// gateway will refresh it within one poll interval.
func (c *remoteCache) Load() (*api.RemoteConfig, error) {
	b, err := os.ReadFile(c.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var got cachedRemote
	if err := json.Unmarshal(b, &got); err != nil {
		return nil, nil
	}
	return &api.RemoteConfig{
		DeviceID:         got.DeviceID,
		BackupPath:       got.BackupPath,
		Status:           api.DeviceState(got.Status),
		ScheduleInterval: got.ScheduleInterval,
		RetryInterval:    got.RetryInterval,
	}, nil
}

// Save records remote, ignoring a failure to write: a cache miss costs one
// poll interval, a failed backup costs the customer's data.
func (c *remoteCache) Save(remote *api.RemoteConfig) error {
	if remote == nil {
		return nil
	}
	out := cachedRemote{
		DeviceID:         remote.DeviceID,
		BackupPath:       remote.BackupPath,
		Status:           string(remote.Status),
		ScheduleInterval: remote.ScheduleInterval,
		RetryInterval:    remote.RetryInterval,
		FetchedAt:        time.Now().UTC().Format(time.RFC3339),
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}
