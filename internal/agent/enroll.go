package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"softafrique-backup-agent/internal/api"
	"softafrique-backup-agent/internal/identity"
	"softafrique-backup-agent/internal/secret"
	"softafrique-backup-agent/internal/status"
)

// Enroll exchanges a one-time token for this device's credentials and seals
// them into the data directory.
//
// The order of operations matters. The blob is written only after the reply
// has passed Validate, and only after it has been proven to work: a device left
// half enrolled, with a repository it cannot open, is worse than one that
// plainly failed to enroll and can be retried with a new token.
func (a *Agent) Enroll(ctx context.Context, token string) (*secret.Credentials, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("an enrollment token is required")
	}
	if err := a.cfg.Validate(); err != nil {
		return nil, err
	}
	if a.secrets.Exists() {
		return nil, errors.New("this device already has credentials; run 'agent unenroll' first if you really mean to start over")
	}

	info := identity.Collect()
	backupPath := a.enrollBackupPath()

	client, err := api.NewUnauthenticated(a.cfg.Server, api.Options{
		UserAgent: "SoftafriqueBackupAgent/" + a.Version,
	})
	if err != nil {
		return nil, err
	}

	a.log.Info("enrolling with the gateway",
		"server", client.BaseURL(),
		"hostname", identity.EnrollHostname(),
		"os", info.OSCaption,
		"agent_version", a.Version,
	)

	resp, err := client.Enroll(ctx, api.EnrollRequest{
		Token:        token,
		Hostname:     identity.EnrollHostname(),
		OSCaption:    info.OSCaption,
		BackupPath:   backupPath,
		AgentVersion: a.Version,
	})
	if err != nil {
		return nil, explainEnrollError(err)
	}
	if err := resp.Validate(); err != nil {
		return nil, fmt.Errorf("the gateway returned an unusable enrollment reply: %w", err)
	}
	if !deviceIDPattern.MatchString(resp.DeviceID) {
		return nil, fmt.Errorf("the gateway assigned the device id %q, which is not a usable identifier; "+
			"this needs fixing gateway-side before this device can be enrolled", resp.DeviceID)
	}

	creds := &secret.Credentials{
		DeviceID:       resp.DeviceID,
		DevicePassword: resp.DevicePassword,
		EncryptionKey:  resp.EncryptionKey,
		Repository:     secret.SanitizeURL(strings.TrimSpace(resp.Repository)),
		Tenant:         resp.Tenant,
		BackupPath:     firstNonEmpty(resp.BackupPath, backupPath),
		Server:         client.BaseURL(),
		EnrolledAt:     status.Now(),
	}
	// Validate catches the two ways this goes wrong in production: a repository
	// URL carrying a password, and a missing key. Both must be refused before
	// anything touches the disk.
	if err := creds.Validate(); err != nil {
		return nil, fmt.Errorf("refusing to store these credentials: %w", err)
	}

	// Prove it works before committing. A wrong key or an unreachable
	// repository should surface now, with the token still in hand, rather than
	// on a scheduled run tomorrow.
	if err := a.probeCredentials(ctx, creds); err != nil {
		return nil, err
	}

	if err := a.secrets.Save(creds); err != nil {
		return nil, fmt.Errorf("could not write the credentials to %s: %w", a.secrets.Path(), err)
	}
	// The plaintext password file from 0.1.0 is now redundant and is a live
	// secret on disk.
	a.forgetLegacyPasswordFile()
	// Seed the remote config cache so a reboot without network still works.
	_ = a.cache.Save(&api.RemoteConfig{
		DeviceID:         creds.DeviceID,
		BackupPath:       creds.BackupPath,
		Status:           api.StateActive,
		ScheduleInterval: api.Duration(a.cfg.ScheduleInterval),
		RetryInterval:    api.Duration(a.cfg.RetryInterval),
	})

	a.log.Info("enrollment complete", "credentials", creds.Redacted())

	// Point the running agent at the new credentials.
	if err := a.load(); err != nil {
		return nil, err
	}
	return creds, nil
}

// probeCredentials checks the repository before the blob is written.
func (a *Agent) probeCredentials(ctx context.Context, creds *secret.Credentials) error {
	client, err := api.New(creds.Server, creds.BasicAuthUser(), creds.DevicePassword, api.Options{
		UserAgent: "SoftafriqueBackupAgent/" + a.Version,
	})
	if err != nil {
		return err
	}
	// Prove the API credentials work: this is what stops the agent reporting
	// itself as enrolled while every /config and /status call 401s.
	if err := client.Health(ctx); err != nil {
		a.log.Warn("the gateway health endpoint did not answer; continuing", "err", err)
	}
	if _, err := client.Config(ctx); err != nil {
		if errors.Is(err, api.ErrUnavailable) {
			a.log.Warn("the gateway is unreachable right now; enrolling anyway because the repository is reachable",
				"err", err)
		} else {
			return fmt.Errorf("the gateway rejected the new device credentials: %w", err)
		}
	}

	runner := a.probeRestic(creds)
	if err := runner.Ping(ctx); err != nil {
		return fmt.Errorf("the assigned repository could not be opened with the issued key: %w", err)
	}
	return nil
}

// withdrawTimeout is deliberately shorter than the client's default. A machine
// being decommissioned often has no network, and an uninstall must not sit for
// the full request timeout waiting for a gateway that will never answer.
const withdrawTimeout = 10 * time.Second

// Withdraw tells the gateway this device is being decommissioned. It has to be
// called while credentials.dat still exists: the endpoint authenticates with the
// device's own credentials, which local removal is about to delete.
//
// The call is best effort by design. A gateway that is unreachable, has already
// archived the device, or refuses the credentials is logged and returned to the
// caller, but is never a reason to fail an uninstall or to keep a key on a
// decommissioned machine. Withdrawal only changes what the gateway accepts; it
// cannot delete the repository, the escrowed key or any snapshot.
func (a *Agent) Withdraw(ctx context.Context) (*api.WithdrawResponse, error) {
	if err := a.load(); err != nil {
		return nil, err
	}
	gw := a.gatewayClient()
	if gw == nil {
		return nil, ErrNotEnrolled
	}
	ctx, cancel := context.WithTimeout(ctx, withdrawTimeout)
	defer cancel()

	resp, err := gw.Withdraw(ctx)
	if err != nil {
		if errors.Is(err, api.ErrRevoked) {
			a.log.Warn("the gateway reports this device is archived; it is already withdrawn", "err", err)
		} else {
			a.log.Warn("could not withdraw this device at the gateway; continuing with local removal", "err", err)
		}
		return nil, err
	}
	if resp == nil {
		resp = &api.WithdrawResponse{}
	}
	a.log.Info("device withdrawn at the gateway",
		"device_id", resp.DeviceID,
		"status", resp.Status,
		"already_withdrawn", resp.AlreadyWithdrawn,
		"note", resp.Note)
	return resp, nil
}

// Unenroll removes the credentials from this device.
//
// It is a local operation by design: it does not call the gateway and does not
// touch the repository, so it can be run on a device with no network. Deleting
// the key is the destructive part and it is not reversible from this machine,
// which is why the CLI makes the operator confirm. Withdrawal on uninstall is a
// separate command precisely so this stays usable for re-enrollment.
func (a *Agent) Unenroll() error {
	if !a.secrets.Exists() {
		return nil
	}
	creds, err := a.secrets.Load()
	if err != nil {
		return err
	}
	deviceID := creds.DeviceID
	if err := a.secrets.Delete(); err != nil {
		return fmt.Errorf("could not remove %s: %w", a.secrets.Path(), err)
	}
	_ = a.store.Update(func(st *status.Status) {
		// Leave the attempt history alone: it is the only record of what this
		// machine was protecting, and an operator reading a post-uninstall
		// report still wants it.
		st.Enrolled = false
		st.SetIdentity("", "", "", "", a.Version, "")
		st.BackupPaths = nil
		st.SetGatewayState("unknown", "")
	})
	a.mu.Lock()
	a.creds = nil
	a.rest = nil
	a.gw = nil
	a.mu.Unlock()

	a.log.Info("credentials removed from this device", "device_id", deviceID,
		"note", "the repository and every snapshot in it are untouched")
	a.log.Warn("the encryption key for this device's repository has been deleted from this machine; " +
		"a restore from this device is no longer possible without the escrowed key")
	return nil
}

// forgetLegacyPasswordFile removes the 0.1.0 plaintext password file.
func (a *Agent) forgetLegacyPasswordFile() {
	path := a.cfg.PasswordFile
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err != nil {
		return
	}
	if !insideDir(a.cfg.DataDir, path) {
		return
	}
	if err := os.Remove(path); err != nil {
		a.log.Warn("could not remove the legacy password file; please delete it by hand", "path", path, "err", err)
		return
	}
	a.log.Info("removed the legacy plaintext password file", "path", path)
}

// errString renders an error for a log field, tolerating nil.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// apiErrorBody returns the gateway's own message, which usually says more than
// the status code does.
func apiErrorBody(err error) string {
	var apiErr *api.Error
	if errors.As(err, &apiErr) && apiErr.Body != "" {
		return apiErr.Body
	}
	return "no detail from the gateway"
}

// explainEnrollError turns an API failure into something an operator reading a
// RMM task result can act on.
func explainEnrollError(err error) error {
	switch {
	case errors.Is(err, api.ErrTokenInvalid):
		return fmt.Errorf("%w; ask for a new token, the previous one cannot be reused", err)
	case errors.Is(err, api.ErrAlreadyEnrolled):
		return fmt.Errorf("%w; this token has already been used. If this is a reinstall, "+
			"unenroll this machine first", err)
	case errors.Is(err, api.ErrBadRequest):
		// A 400 is whichever the gateway chose to use for either of these, so
		// name both rather than guessing and sending the technician down the
		// wrong path.
		return fmt.Errorf("%w; the usual causes are a token the gateway will not "+
			"accept or a machine name it rejects: %s", err, apiErrorBody(err))
	case errors.Is(err, api.ErrUnavailable):
		return fmt.Errorf("%w; check outbound access to %s, then run enroll again with the same token if it has not been consumed",
			err, api.DefaultBaseURL)
	default:
		return err
	}
}
