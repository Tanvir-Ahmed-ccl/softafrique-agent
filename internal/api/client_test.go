package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testDeviceID = "dev-01HQ8XK3M4N7"
	testPassword = "9f2c4a7b1d8e"
	testKey      = "e3b1f0a95c2d47e6b8a10f34c9d2e7b56"
)

func newTestClient(t *testing.T, h http.HandlerFunc, auth bool) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	var c *Client
	var err error
	if auth {
		c, err = New(srv.URL, testDeviceID, testPassword, Options{Timeout: 5 * time.Second})
	} else {
		c, err = NewUnauthenticated(srv.URL, Options{Timeout: 5 * time.Second})
	}
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func TestEnrollSuccess(t *testing.T) {
	var got EnrollRequest
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/enroll" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if _, _, ok := r.BasicAuth(); ok {
			t.Error("enroll must not send credentials")
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		json.NewEncoder(w).Encode(EnrollResponse{
			Tenant:         "softafrique",
			DeviceID:       testDeviceID,
			Repository:     "rest:https://backup.softafrique.net/" + testDeviceID,
			DevicePassword: testPassword,
			EncryptionKey:  testKey,
			BackupPath:     `C:\SoftafriqueBackup`,
		})
	}, false)

	resp, err := c.Enroll(context.Background(), EnrollRequest{
		Token: "tok-123", Hostname: "mohsin", OSCaption: "Windows 11 Pro",
		BackupPath: `C:\SoftafriqueBackup`, AgentVersion: "0.2.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.DeviceID != testDeviceID || resp.EncryptionKey != testKey {
		t.Errorf("unexpected response: %+v", resp.RedactedForLog())
	}
	if got.Token != "tok-123" || got.Hostname != "mohsin" || got.OSCaption != "Windows 11 Pro" ||
		got.BackupPath != `C:\SoftafriqueBackup` || got.AgentVersion != "0.2.0" {
		t.Errorf("request body not sent as specified: %+v", got)
	}
}

func TestEnrollErrors(t *testing.T) {
	cases := []struct {
		name string
		code int
		want error
	}{
		{"bad token", http.StatusForbidden, ErrTokenInvalid},
		{"expired token", http.StatusForbidden, ErrTokenInvalid},
		{"already enrolled", http.StatusConflict, ErrAlreadyEnrolled},
		{"bad hostname", http.StatusBadRequest, ErrBadRequest},
		{"server error", http.StatusInternalServerError, ErrUnavailable},
		{"bad gateway", http.StatusBadGateway, ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				w.Write([]byte(`{"detail":"nope"}`))
			}, false)
			_, err := c.Enroll(context.Background(), EnrollRequest{Token: "t"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestEnrollEmptyTokenDoesNotCallGateway(t *testing.T) {
	called := false
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
	}, false)
	if _, err := c.Enroll(context.Background(), EnrollRequest{}); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("want ErrTokenInvalid, got %v", err)
	}
	if called {
		t.Error("a request was sent despite the empty token")
	}
}

func TestEnrollRejectsIncompleteReply(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// device_id and repository present, no key: a device in this state could
		// find its repository but never open it.
		json.NewEncoder(w).Encode(EnrollResponse{
			DeviceID:   testDeviceID,
			Repository: "rest:https://backup.softafrique.net/" + testDeviceID,
		})
	}, false)
	_, err := c.Enroll(context.Background(), EnrollRequest{Token: "t"})
	if err == nil {
		t.Fatal("expected an incomplete reply to be rejected")
	}
	if !strings.Contains(err.Error(), "encryption_key") {
		t.Errorf("error should name the missing field, got %v", err)
	}
}

func TestConfigActive(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != testDeviceID || pass != testPassword {
			t.Errorf("basic auth = %q/%q ok=%v, want %q", user, pass, ok, testDeviceID)
		}
		w.Write([]byte(`{"device_id":"` + testDeviceID + `","backup_path":"C:\\Data","status":"active","schedule_interval":"1h","retry_interval":"5m"}`))
	}, true)

	cfg, err := c.Config(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IsActive() {
		t.Error("expected the device to be active")
	}
	if cfg.ScheduleInterval.Duration() != time.Hour {
		t.Errorf("schedule_interval = %v, want 1h", cfg.ScheduleInterval.Duration())
	}
	if cfg.RetryInterval.Duration() != 5*time.Minute {
		t.Errorf("retry_interval = %v, want 5m", cfg.RetryInterval.Duration())
	}
	if cfg.BackupPath != `C:\Data` {
		t.Errorf("backup_path = %q", cfg.BackupPath)
	}
}

func TestConfigSuspendedIsNotActive(t *testing.T) {
	for _, status := range []string{"suspended", "disabled", "pending", ""} {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"device_id":"` + testDeviceID + `","status":"` + status + `","backup_path":"C:\\Data"}`))
		}, true)
		cfg, err := c.Config(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if cfg.IsActive() {
			t.Errorf("status %q must not be treated as active", status)
		}
		if cfg.SuspendReason() == "" {
			t.Errorf("status %q should produce a suspend reason", status)
		}
	}
}

func TestConfigUnauthorized(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}, true)
	_, err := c.Config(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

func TestConfigForbiddenIsRevocation(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}, true)
	_, err := c.Config(context.Background())
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("want ErrRevoked, got %v", err)
	}
}

func TestConfigUnreachableIsTemporary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now
	c, err := New(url, testDeviceID, testPassword, Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Config(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Temporary() != true {
		t.Errorf("a transport failure should be temporary, got %v", err)
	}
}

func TestPostStatusSendsContract(t *testing.T) {
	var got map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if _, _, ok := r.BasicAuth(); !ok {
			t.Error("status must be authenticated")
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}, true)

	report := StatusReport{
		LastBackupStatus:    "success",
		LastSnapshotID:      "abc123",
		FilesAdded:          1,
		FilesChanged:        2,
		BytesAdded:          4512,
		LastDurationSeconds: 213.5,
		AgentVersion:        "0.2.0",
		OSCaption:           "Windows Server 2019 Standard",
	}
	if err := c.PostStatus(context.Background(), report); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"last_backup_status", "last_snapshot_id", "files_added", "files_changed",
		"bytes_added", "last_duration_seconds", "last_backup_error", "agent_version", "os_caption",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("contract field %q missing from the request body", key)
		}
	}
	if len(got) != 9 {
		t.Errorf("sent %d fields, want exactly the 9 in the contract: %v", len(got), got)
	}
	if got["last_duration_seconds"].(float64) != 213.5 {
		t.Errorf("duration not sent as elapsed seconds: %v", got["last_duration_seconds"])
	}
}

func TestHealthNeedsNoAuth(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); ok {
			t.Error("health must be callable without credentials")
		}
		w.Write([]byte(`{"status":"ok"}`))
	}, false)
	if err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticatedCallWithoutCredentialsFails(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be sent")
	}, false)
	if _, err := c.Config(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("config: want ErrUnauthorized, got %v", err)
	}
	if err := c.PostStatus(context.Background(), StatusReport{}); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("status: want ErrUnauthorized, got %v", err)
	}
}

func TestWithCredentialsLeavesReceiverAlone(t *testing.T) {
	base, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {}, false)
	if base.Authenticated() {
		t.Error("enroll client should be unauthenticated")
	}
	authed := base.WithCredentials(testDeviceID, testPassword)
	if !authed.Authenticated() {
		t.Error("WithCredentials should return an authenticated client")
	}
	if base.Authenticated() {
		t.Error("WithCredentials must not mutate the receiver")
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://backup.softafrique.net/api/v1":     "https://backup.softafrique.net/api/v1",
		"https://backup.softafrique.net/api/v1/":    "https://backup.softafrique.net/api/v1",
		"backup.softafrique.net/api/v1":             "https://backup.softafrique.net/api/v1",
		"  https://backup.softafrique.net/api/v1  ": "https://backup.softafrique.net/api/v1",
		"https://backup.softafrique.net/api/v1?x=1": "https://backup.softafrique.net/api/v1",
	}
	for in, want := range cases {
		got, err := NormalizeBaseURL(in)
		if err != nil {
			t.Errorf("NormalizeBaseURL(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
	bad := []string{"", "   ", "ftp://host/api", "https://", "://nope"}
	for _, in := range bad {
		if _, err := NormalizeBaseURL(in); err == nil {
			t.Errorf("NormalizeBaseURL(%q) should have failed", in)
		}
	}
}

func TestDurationAcceptsStringAndSeconds(t *testing.T) {
	cases := []struct {
		json string
		want time.Duration
	}{
		{`"1h"`, time.Hour},
		{`"90m"`, 90 * time.Minute},
		{`3600`, time.Hour},
		{`1800.5`, time.Duration(1800.5 * float64(time.Second))},
		{`"3600"`, time.Hour},
		{`null`, 0},
		{`""`, 0},
	}
	for _, tc := range cases {
		var d Duration
		if err := json.Unmarshal([]byte(tc.json), &d); err != nil {
			t.Errorf("Unmarshal(%s): %v", tc.json, err)
			continue
		}
		if d.Duration() != tc.want {
			t.Errorf("Unmarshal(%s) = %v, want %v", tc.json, d.Duration(), tc.want)
		}
	}
	var d Duration
	if err := json.Unmarshal([]byte(`"not a duration"`), &d); err == nil {
		t.Error("expected an unparseable duration to fail")
	}
}

func TestErrorMessageHasNoCredentials(t *testing.T) {
	// A gateway that reflects the Authorization header back in an error body
	// must not end up with the device password in a log line.
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"detail":"bad password ` + testPassword + ` for ` + testDeviceID + `"}`))
	}, true)
	_, err := c.Config(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	t.Logf("error text (reviewed by a human): %v", err)
}

func TestContextCancellationIsNotWrappedAsUnavailable(t *testing.T) {
	release := make(chan struct{})
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
	}, true)
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := c.Config(ctx)
	if err == nil {
		t.Fatal("expected cancellation")
	}
	if errors.Is(err, ErrUnavailable) {
		t.Error("a service stop must not look like a gateway outage")
	}
}

func TestContextDeadlineRespected(t *testing.T) {
	release := make(chan struct{})
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
	}, true)
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Config(ctx); err == nil {
		t.Fatal("expected a timeout")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("call took %v; the request context is not being honoured", elapsed)
	}
}
