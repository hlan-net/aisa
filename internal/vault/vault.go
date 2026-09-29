// Package vault is the small part of Vault's HTTP API that aisa uses: logging in with a
// Kubernetes service account token (or using a given token) and reading a KV v2 mount.
//
// It uses the standard library only. The official client would bring a large dependency tree
// for three endpoints, and aisa runs on small arm64 nodes.
package vault

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned when a path has no data: a secret that does not exist or whose
// current version is deleted, or a path without keys. It is what Vault itself says about its
// data. A 404 for any other reason, such as a mount that does not exist or an answer from a
// proxy in front of Vault, is a StatusError.
var ErrNotFound = errors.New("not found")

// Auth obtains a Vault token.
type Auth interface {
	// Login returns a token and how long it is valid; 0 means it does not expire.
	Login(ctx context.Context, c *Client) (token string, ttl time.Duration, err error)
}

// TokenAuth uses a fixed token, as in development with a dev-mode Vault.
type TokenAuth struct{ Token string }

// Login returns the fixed token.
func (a TokenAuth) Login(context.Context, *Client) (string, time.Duration, error) {
	if a.Token == "" {
		return "", 0, errors.New("empty token")
	}
	return a.Token, 0, nil
}

// KubernetesAuth logs in with the pod's service account token through Vault's Kubernetes auth
// method.
type KubernetesAuth struct {
	// Mount is the auth method's mount path, "kubernetes" by default.
	Mount string
	// Role is the Vault role to log in as.
	Role string
	// TokenPath is the service account token file.
	TokenPath string
}

// Login reads the service account token and exchanges it for a Vault token.
func (a KubernetesAuth) Login(ctx context.Context, c *Client) (string, time.Duration, error) {
	jwt, err := os.ReadFile(a.TokenPath)
	if err != nil {
		return "", 0, fmt.Errorf("read service account token: %w", err)
	}
	body, err := json.Marshal(map[string]string{"role": a.Role, "jwt": strings.TrimSpace(string(jwt))})
	if err != nil {
		return "", 0, fmt.Errorf("encode login request: %w", err)
	}
	var out struct {
		Auth *struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := c.do(ctx, http.MethodPost, "auth/"+a.Mount+"/login", "", body, &out); err != nil {
		return "", 0, fmt.Errorf("kubernetes login as role %q: %w", a.Role, err)
	}
	if out.Auth == nil || out.Auth.ClientToken == "" {
		return "", 0, errors.New("kubernetes login: no token in the answer")
	}
	return out.Auth.ClientToken, time.Duration(out.Auth.LeaseDuration) * time.Second, nil
}

// Client talks to one Vault server.
type Client struct {
	addr string
	http *http.Client
	auth Auth
	now  func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time // zero: does not expire
}

// Options configure a Client.
type Options struct {
	// Addr is Vault's address, such as https://vault.example:8200.
	Addr string
	// CACert is a PEM file with the CA that signed Vault's certificate; empty uses the system pool.
	CACert string
	// Auth obtains the token.
	Auth Auth
	// Timeout bounds each HTTP request; 10 s when zero.
	Timeout time.Duration
	// Now returns the current time; tests replace it.
	Now func() time.Time
}

// New returns a client. It does not contact Vault; the first request logs in.
func New(o Options) (*Client, error) {
	u, err := url.Parse(o.Addr)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("vault address: want http(s)://host[:port], got %q", o.Addr)
	}
	if o.Auth == nil {
		return nil, errors.New("vault: no auth method")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if o.CACert != "" {
		pem, err := os.ReadFile(o.CACert)
		if err != nil {
			return nil, fmt.Errorf("read vault CA certificate: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("vault CA certificate %s: no PEM certificate found", o.CACert)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		addr: strings.TrimRight(o.Addr, "/"),
		http: &http.Client{Transport: transport, Timeout: timeout},
		auth: o.Auth,
		now:  now,
	}, nil
}

// List returns the keys under a KV v2 path, such as the consumers under "aisa/consumers".
// Subdirectories end in "/". A path without keys returns ErrNotFound.
func (c *Client) List(ctx context.Context, mount, path string) ([]string, error) {
	var out struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := c.authed(ctx, "LIST", mount+"/metadata/"+path, nil, &out); err != nil {
		return nil, err
	}
	return out.Data.Keys, nil
}

// Read returns the current version of a KV v2 secret's data. A deleted or destroyed version
// returns ErrNotFound.
func (c *Client) Read(ctx context.Context, mount, path string) (map[string]any, error) {
	var out struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := c.authed(ctx, http.MethodGet, mount+"/data/"+path, nil, &out); err != nil {
		return nil, err
	}
	if out.Data.Data == nil {
		return nil, ErrNotFound
	}
	return out.Data.Data, nil
}

// lookupSelf is allowed to every token by Vault's default policy.
const lookupSelf = "auth/token/lookup-self"

// Probe checks that Vault answers and accepts aisa's token, logging in when it has none.
func (c *Client) Probe(ctx context.Context) error {
	return c.authed(ctx, http.MethodGet, lookupSelf, nil, nil)
}

// authed performs a request with a valid token. Vault answers 403 both to a token that was
// revoked or has expired early and to a valid token whose policy does not allow the path. Only
// the first is helped by logging in again, so it asks Vault about the token before it does:
// a login on every denied request would leave a token behind each time.
func (c *Client) authed(ctx context.Context, method, path string, body []byte, out any) error {
	token, err := c.currentToken(ctx, "")
	if err != nil {
		return err
	}
	err = c.do(ctx, method, path, token, body, out)
	if !isForbidden(err) {
		return err
	}
	// When the request was that question itself, its answer is the token's state.
	if path != lookupSelf && c.tokenState(ctx, token) != tokenInvalid {
		return err
	}
	fresh, lerr := c.currentToken(ctx, token)
	if lerr != nil {
		return lerr
	}
	if fresh == token {
		// A fixed token: there is no other one to try.
		return err
	}
	return c.do(ctx, method, path, fresh, body, out)
}

type tokenState int

const (
	tokenValid tokenState = iota
	tokenInvalid
	tokenUnknown // Vault did not answer the question
)

// tokenState asks Vault whether it accepts the token.
func (c *Client) tokenState(ctx context.Context, token string) tokenState {
	switch err := c.do(ctx, http.MethodGet, lookupSelf, token, nil, nil); {
	case err == nil:
		return tokenValid
	case isForbidden(err):
		return tokenInvalid
	default:
		return tokenUnknown
	}
}

func isForbidden(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusForbidden
}

// currentToken returns the cached token, logging in when there is none or when it expires
// within a third of its lifetime or a minute. stale is a token that Vault has rejected: it logs
// in again unless another request has replaced that token in the meantime.
func (c *Client) currentToken(ctx context.Context, stale string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.token != stale && (c.expires.IsZero() || c.now().Before(c.expires)) {
		return c.token, nil
	}
	token, ttl, err := c.auth.Login(ctx, c)
	if err != nil {
		return "", fmt.Errorf("vault login: %w", err)
	}
	c.token = token
	c.expires = time.Time{}
	if ttl > 0 {
		margin := min(ttl/3, time.Minute)
		c.expires = c.now().Add(ttl - margin)
	}
	return token, nil
}

// StatusError is an answer from Vault with an unexpected HTTP status.
type StatusError struct {
	Code   int
	Errors []string
}

func (e *StatusError) Error() string {
	if len(e.Errors) == 0 {
		return fmt.Sprintf("vault answered %d", e.Code)
	}
	return fmt.Sprintf("vault answered %d: %s", e.Code, strings.Join(e.Errors, "; "))
}

// escapePath escapes each element of a path, so a name with a question mark, a number sign or
// a percent sign in it stays a name and does not change which path is asked for.
func escapePath(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func (c *Client) do(ctx context.Context, method, path, token string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.addr+"/v1/"+escapePath(path), rd)
	if err != nil {
		return fmt.Errorf("vault request %s: %w", path, err)
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("vault %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("vault %s %s: read answer: %w", method, path, err)
	}

	switch {
	case resp.StatusCode == http.StatusNoContent:
		return nil
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		errs, noData := errorAnswer(data)
		if resp.StatusCode == http.StatusNotFound && noData {
			return fmt.Errorf("vault %s %s: %w", method, path, ErrNotFound)
		}
		return &StatusError{Code: resp.StatusCode, Errors: errs}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("vault %s %s: decode answer: %w", method, path, err)
	}
	return nil
}

// errorBody is the body of an error answer from Vault. The pointers tell a missing field from an
// empty one.
type errorBody struct {
	Errors *[]string       `json:"errors"`
	Data   *deletedVersion `json:"data"`
}

// deletedVersion is the data of a KV v2 read of a deleted or destroyed version.
type deletedVersion struct {
	Metadata *versionMetadata `json:"metadata"`
}

type versionMetadata struct {
	DeletionTime string `json:"deletion_time"`
	Destroyed    bool   `json:"destroyed"`
}

// errorAnswer reads the body of an error from Vault: its errors, and whether it is Vault's way of
// saying "no data here". That is an empty errors list, or the metadata of a deleted or destroyed
// version.
// With errors, a 404 is about the mount. Any other answer, a JSON one from a proxy included, is
// not from Vault.
func errorAnswer(data []byte) (errs []string, noData bool) {
	var e errorBody
	if json.Unmarshal(data, &e) != nil {
		return nil, false
	}
	if e.Errors != nil {
		return *e.Errors, len(*e.Errors) == 0
	}
	if e.Data == nil || e.Data.Metadata == nil {
		return nil, false
	}
	return nil, e.Data.Metadata.DeletionTime != "" || e.Data.Metadata.Destroyed
}
