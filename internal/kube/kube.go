// Package kube is a small, read-only client for the API server of the cluster
// a run created. It only sends GET requests: no write method exists, which is
// the code-level restriction described as ReadOnlyCluster in
// docs/architecture/overview.md. It is not an RBAC or security boundary.
//
// It deliberately has no dependency on client-go. It reads only the kubeconfig
// that Kind writes for the run (one cluster, embedded client certificate and
// key) and refuses anything else, so it can never be pointed at another cluster.
package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrInvalid is wrapped by every error that rejects a kubeconfig or a request
// before anything is sent.
var ErrInvalid = errors.New("invalid Kubernetes client request")

// ErrTooLarge is wrapped when a response is bigger than the configured cap.
var ErrTooLarge = errors.New("API response is too large")

const (
	defaultTimeout     = 30 * time.Second
	defaultMaxResponse = 32 << 20
	messageLimit       = 1024
)

// Options tune a Client. The zero value uses a 30 second timeout per request
// and a 32 MiB response cap.
type Options struct {
	Timeout     time.Duration
	MaxResponse int64
}

// Client talks to one API server. Create it with New.
type Client struct {
	base        string
	http        *http.Client
	transport   *http.Transport
	maxResponse int64
}

var _ interface{ Ready(context.Context) error } = (*Client)(nil)

// StatusError is returned when the API server answers with a status outside 2xx.
type StatusError struct {
	Code    int
	Reason  string
	Message string
}

func (e *StatusError) Error() string {
	s := fmt.Sprintf("Kubernetes API returned %d", e.Code)
	if e.Reason != "" {
		s += " " + e.Reason
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// IsNotFound reports whether err is a 404 answer.
func IsNotFound(err error) bool { return hasCode(err, http.StatusNotFound) }

// IsForbidden reports whether err is a 403 answer.
func IsForbidden(err error) bool { return hasCode(err, http.StatusForbidden) }

// IsUnauthorized reports whether err is a 401 answer.
func IsUnauthorized(err error) bool { return hasCode(err, http.StatusUnauthorized) }

func hasCode(err error, code int) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == code
}

// Get sends GET path and decodes the JSON answer into out. A nil out discards
// the body. path starts with a single slash and may carry a query string.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	status, body, err := c.do(ctx, path)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return statusError(status, body)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("GET %s: cannot decode the response: %w", path, err)
	}
	return nil
}

// Ready probes /readyz and returns nil when the API server is ready. It is the
// probe the Helm package uses to tell a broken cluster from a broken chart.
func (c *Client) Ready(ctx context.Context) error {
	status, body, err := c.do(ctx, "/readyz")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return statusError(status, body)
	}
	return nil
}

// Close releases idle connections.
func (c *Client) Close() { c.transport.CloseIdleConnections() }

func (c *Client) do(ctx context.Context, path string) (int, []byte, error) {
	if err := checkPath(path); err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	req.Header.Set("Accept", "application/json, */*")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponse+1))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("GET %s: reading the response: %w", path, err)
	}
	if int64(len(body)) > c.maxResponse {
		return resp.StatusCode, nil, fmt.Errorf("GET %s: %w (more than %d bytes)", path, ErrTooLarge, c.maxResponse)
	}
	return resp.StatusCode, body, nil
}

func checkPath(p string) error {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, " \t\r\n#\\") {
		return fmt.Errorf("%w: path %q must start with a single slash and hold no spaces", ErrInvalid, p)
	}
	return nil
}

func statusError(code int, body []byte) *StatusError {
	e := &StatusError{Code: code}
	var s struct {
		Kind    string `json:"kind"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &s) == nil && s.Kind == "Status" {
		e.Reason = s.Reason
		e.Message = clip(s.Message)
		return e
	}
	e.Message = clip(string(body))
	return e
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > messageLimit {
		s = strings.ToValidUTF8(s[:messageLimit], "") + "..."
	}
	return s
}
