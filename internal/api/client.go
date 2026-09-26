package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to the gateway API.
type Client struct {
	baseURL string
	http    *http.Client
	// user and pass are the HTTP basic credentials. Empty for the
	// unauthenticated enroll and health calls.
	user string
	pass string
	// ua is the User-Agent header value.
	ua string
}

// Options tune a Client. The zero value is appropriate for production.
type Options struct {
	// Timeout bounds a single request including the body read. Enrollment gets
	// a longer default because it is a one-off interactive operation.
	Timeout time.Duration
	// UserAgent identifies the agent build to the gateway.
	UserAgent string
}

// New returns a Client for baseURL authenticated as deviceID/devicePassword.
// baseURL may be given with or without a scheme and with or without a trailing
// slash.
func New(baseURL, deviceID, devicePassword string, opts Options) (*Client, error) {
	normalized, err := NormalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	return &Client{
		baseURL: normalized,
		http:    newHTTPClient(opts.Timeout),
		user:    deviceID,
		pass:    devicePassword,
		ua:      opts.UserAgent,
	}, nil
}

// NewUnauthenticated returns a Client for the one-time enroll call and the
// health probe, which need no credentials.
func NewUnauthenticated(baseURL string, opts Options) (*Client, error) {
	return New(baseURL, "", "", opts)
}

// BaseURL is the normalized API root, safe to log.
func (c *Client) BaseURL() string { return c.baseURL }

// Authenticated reports whether the client carries device credentials.
func (c *Client) Authenticated() bool { return c.user != "" }

// WithCredentials returns a copy of the client authenticated as the given
// device. The receiver is left alone so a client built for enrollment can be
// reused for the authenticated calls afterwards.
func (c *Client) WithCredentials(deviceID, devicePassword string) *Client {
	out := *c
	out.user = deviceID
	out.pass = devicePassword
	return &out
}

// NormalizeBaseURL validates and canonicalises the gateway base URL.
func NormalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("gateway base URL is empty")
	}
	if !strings.Contains(raw, "://") {
		// Default to https: the agent must not silently fall back to plaintext
		// credentials on a link that carries the device password.
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid gateway base URL %q: %w", raw, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("gateway base URL must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("gateway base URL %q has no host", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// newHTTPClient builds the transport.
//
// MinVersion TLS 1.2 is the floor, which is what makes the agent work on
// Windows Server 2016 and 2019 without the .NET StrongCrypto registry keys
// Mohsin had to set: those keys change the .NET stack's defaults, and the Go
// client does not use .NET. InsecureSkipVerify is never set and there is no
// way to configure one, so TLS verification cannot be turned off by a config
// file.
func newHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			MaxIdleConns:          8,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// A redirect would resend the Authorization header to whatever host
			// the gateway names. Refuse rather than leak the device password.
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			if req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("refusing cross-host redirect to %s", req.URL.Host)
			}
			return nil
		},
	}
}

// Enroll exchanges a one-time token for this device's credentials.
func (c *Client) Enroll(ctx context.Context, req EnrollRequest) (*EnrollResponse, error) {
	if strings.TrimSpace(req.Token) == "" {
		return nil, &Error{Op: "enroll", Err: ErrTokenInvalid, Body: "no token supplied"}
	}
	var out EnrollResponse
	if err := c.do(ctx, http.MethodPost, "/enroll", req, &out, false); err != nil {
		return nil, err
	}
	if err := out.Validate(); err != nil {
		return nil, &Error{Op: "enroll", Path: "/enroll", Err: err}
	}
	return &out, nil
}

// Config fetches the device's current remote configuration.
func (c *Client) Config(ctx context.Context) (*RemoteConfig, error) {
	if !c.Authenticated() {
		return nil, &Error{Op: "config", Path: "/config", Err: ErrUnauthorized, Body: "client has no device credentials"}
	}
	var out RemoteConfig
	if err := c.do(ctx, http.MethodGet, "/config", nil, &out, true); err != nil {
		return nil, err
	}
	return &out, nil
}

// PostStatus reports the outcome of a backup attempt.
func (c *Client) PostStatus(ctx context.Context, report StatusReport) error {
	if !c.Authenticated() {
		return &Error{Op: "status", Path: "/status", Err: ErrUnauthorized, Body: "client has no device credentials"}
	}
	return c.do(ctx, http.MethodPost, "/status", report, nil, true)
}

// Health probes the gateway. It is unauthenticated and is used by the RMM
// scripts and `agent doctor`.
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/health", nil, nil, false)
}

// do performs one request. auth selects whether the basic credentials are
// attached.
func (c *Client) do(ctx context.Context, method, path string, in, out any, auth bool) error {
	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return &Error{Op: opName(path), Method: method, Path: path, Err: err}
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return &Error{Op: opName(path), Method: method, Path: path, Err: err}
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.ua != "" {
		req.Header.Set("User-Agent", c.ua)
	}
	if auth && c.Authenticated() {
		req.SetBasicAuth(c.user, c.pass)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// A context cancellation is the caller's decision (service stop), not a
		// gateway failure, so it is passed through unwrapped.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return &Error{Op: opName(path), Method: method, Path: path, Err: fmt.Errorf("%w: %v", ErrUnavailable, err)}
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if readErr != nil {
			return &Error{Op: opName(path), Method: method, Path: path, StatusCode: resp.StatusCode,
				Err: fmt.Errorf("%w: reading response: %v", ErrUnavailable, readErr)}
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return &Error{Op: opName(path), Method: method, Path: path, StatusCode: resp.StatusCode,
				Err: fmt.Errorf("invalid JSON in reply: %w", err), Body: snippet(raw)}
		}
		return nil
	}
	return &Error{
		Op:         opName(path),
		Method:     method,
		Path:       path,
		StatusCode: resp.StatusCode,
		Body:       snippet(raw),
		Err:        classify(resp.StatusCode, path),
	}
}

// classify maps a status code to a sentinel the agent can branch on.
func classify(code int, path string) error {
	switch {
	case code == http.StatusBadRequest:
		return ErrBadRequest
	case code == http.StatusUnauthorized:
		return ErrUnauthorized
	case code == http.StatusForbidden:
		// A forbidden enroll means the token is bad, expired or spent. A
		// forbidden authenticated call means the device was withdrawn.
		if path == "/enroll" {
			return ErrTokenInvalid
		}
		return ErrRevoked
	case code == http.StatusConflict:
		return ErrAlreadyEnrolled
	case code == http.StatusNotFound:
		return ErrBadRequest
	case code >= 500:
		return ErrUnavailable
	default:
		return ErrBadRequest
	}
}

func opName(path string) string {
	switch path {
	case "/enroll":
		return "enroll"
	case "/config":
		return "fetch config"
	case "/status":
		return "post status"
	case "/health":
		return "health"
	default:
		return path
	}
}

// snippet trims a body down to something safe to put in an error message.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}
