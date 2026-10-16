// Package consul is the small part of Consul's HTTP API that aisa uses: reading a KV prefix,
// with blocking queries so that a change reaches aisa without polling.
//
// It uses the standard library only, as the vault package does: the official client would
// bring a large dependency tree for one endpoint.
package consul

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
	"strconv"
	"strings"
	"time"
)

// Pair is a key and its value in Consul KV.
type Pair struct {
	Key   string
	Value []byte
}

const (
	headerConsulToken = "X-Consul-Token"
	kvPrefix          = "/v1/kv/"
)

// Options configure a Client.
type Options struct {
	// Addr is Consul's address: host:port, or a URL such as https://consul.example:8501.
	// Without a scheme it is http, as with Consul's own CONSUL_HTTP_ADDR.
	Addr string
	// CACert is a PEM file with the CA that signed Consul's certificate; empty uses the system
	// pool.
	CACert string
	// Token is an ACL token. TokenFile is a file that holds one, read again for every request,
	// so a token that the Vault Agent renews there is picked up. At most one of them is set.
	Token     string
	TokenFile string
}

// Client talks to one Consul agent or server.
type Client struct {
	addr      string
	http      *http.Client
	token     string
	tokenFile string
}

// New returns a client. It does not contact Consul.
func New(o Options) (*Client, error) {
	addr := o.Addr
	if !strings.Contains(addr, "://") {
		// Consul's own default without a scheme; set https in CONSUL_HTTP_ADDR for TLS.
		addr = (&url.URL{Scheme: "http", Host: addr}).String()
	}
	u, err := url.Parse(addr)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("consul address: want host:port or http(s)://host:port, got %q", o.Addr)
	}
	if o.Token != "" && o.TokenFile != "" {
		return nil, errors.New("consul: set a token or a token file, not both")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if o.CACert != "" {
		pem, err := os.ReadFile(o.CACert)
		if err != nil {
			return nil, fmt.Errorf("read consul CA certificate: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("consul CA certificate %s: no PEM certificate found", o.CACert)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Client{
		addr: strings.TrimRight(addr, "/"),
		// No client timeout: a blocking query takes as long as its wait, and List bounds each
		// request by that.
		http:      &http.Client{Transport: transport},
		token:     o.Token,
		tokenFile: o.TokenFile,
	}, nil
}

// StatusError is an answer from Consul with an unexpected HTTP status.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("consul answered %d", e.Code)
	}
	return fmt.Sprintf("consul answered %d: %s", e.Code, e.Body)
}

// errNotConsul: the answer lacks the X-Consul-Index header that Consul always sends, so it may
// come from a proxy in front of Consul and cannot pass for an empty prefix.
var errNotConsul = errors.New("answer has no X-Consul-Index, so it is not from Consul")

// ErrConflict is returned when a check-and-set condition is not met.
var ErrConflict = errors.New("consul check-and-set conflict")

// List returns the keys under prefix and their values, and the prefix's index. With index 0 it
// answers at once. With the index of an earlier answer it is a blocking query: Consul answers
// when something under the prefix changes, or after wait. A prefix without keys is an empty
// list.
func (c *Client) List(ctx context.Context, prefix string, index uint64, wait time.Duration) ([]Pair, uint64, error) {
	q := url.Values{"recurse": {"true"}}
	if index > 0 {
		q.Set("index", strconv.FormatUint(index, 10))
		q.Set("wait", strconv.FormatInt(int64(wait/time.Second), 10)+"s")
	}
	// Consul adds up to wait/16 to spread the answers of many watchers.
	ctx, cancel := context.WithTimeout(ctx, wait+wait/16+10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.addr+kvPrefix+escapePath(prefix)+"?"+q.Encode(), nil)
	if err != nil {
		return nil, 0, fmt.Errorf("consul request %s: %w", prefix, err)
	}
	token, err := c.currentToken()
	if err != nil {
		return nil, 0, err
	}
	if token != "" {
		req.Header.Set(headerConsulToken, token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("consul GET kv/%s: %w", prefix, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("consul GET kv/%s: read answer: %w", prefix, err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return nil, 0, fmt.Errorf("consul GET kv/%s: %w", prefix,
			&StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(data[:min(len(data), 512)]))})
	}
	raw := resp.Header.Get("X-Consul-Index")
	if raw == "" {
		return nil, 0, fmt.Errorf("consul GET kv/%s: status %d: %w", prefix, resp.StatusCode, errNotConsul)
	}
	newIndex, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return nil, 0, fmt.Errorf("consul GET kv/%s: X-Consul-Index %q: %w", prefix, raw, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return []Pair{}, newIndex, nil
	}
	var entries []struct {
		Key   string
		Value []byte // base64 in the answer; null for a key without a value
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, 0, fmt.Errorf("consul GET kv/%s: decode answer: %w", prefix, err)
	}
	pairs := make([]Pair, 0, len(entries))
	for _, e := range entries {
		pairs = append(pairs, Pair{Key: e.Key, Value: e.Value})
	}
	return pairs, newIndex, nil
}

// Instance is one registration of a service in the catalog, with the state of its checks.
type Instance struct {
	ID      string            `json:"ID"`
	Service string            `json:"Service"`
	Tags    []string          `json:"Tags"`
	Address string            `json:"Address"`
	Port    int               `json:"Port"`
	Meta    map[string]string `json:"Meta"`
	// Passing is true when every check of the instance and of its node passes.
	Passing bool    `json:"Passing"`
	Checks  []Check `json:"Checks"`
}

// Check is a check of a service instance in Consul.
type Check struct {
	CheckID string `json:"CheckID"`
	Name    string `json:"Name"`
	Status  string `json:"Status"`
	Output  string `json:"Output"`
}

// Health returns the instances of a service, healthy or not, from /v1/health/service. The ACL
// token needs read on the service and on the nodes; Consul leaves out the instances on nodes
// it may not read.
func (c *Client) Health(ctx context.Context, service string) ([]Instance, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.addr+"/v1/health/service/"+url.PathEscape(service), nil)
	if err != nil {
		return nil, fmt.Errorf("consul request health/service/%s: %w", service, err)
	}
	token, err := c.currentToken()
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("X-Consul-Token", token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("consul GET health/service/%s: %w", service, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("consul GET health/service/%s: read answer: %w", service, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("consul GET health/service/%s: %w", service,
			&StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(data[:min(len(data), 512)]))})
	}
	if resp.Header.Get("X-Consul-Index") == "" {
		return nil, fmt.Errorf("consul GET health/service/%s: %w", service, errNotConsul)
	}
	var entries []struct {
		Service struct {
			ID      string            `json:"ID"`
			Service string            `json:"Service"`
			Tags    []string          `json:"Tags"`
			Address string            `json:"Address"`
			Port    int               `json:"Port"`
			Meta    map[string]string `json:"Meta"`
		} `json:"Service"`
		Checks []struct {
			CheckID string `json:"CheckID"`
			Name    string `json:"Name"`
			Status  string `json:"Status"`
			Output  string `json:"Output"`
		} `json:"Checks"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("consul GET health/service/%s: decode answer: %w", service, err)
	}
	out := make([]Instance, 0, len(entries))
	for _, e := range entries {
		passing := true
		checks := make([]Check, 0, len(e.Checks))
		for _, ch := range e.Checks {
			if ch.Status != "passing" {
				passing = false
			}
			checks = append(checks, Check{
				CheckID: ch.CheckID,
				Name:    ch.Name,
				Status:  ch.Status,
				Output:  ch.Output,
			})
		}
		out = append(out, Instance{
			ID:      e.Service.ID,
			Service: e.Service.Service,
			Tags:    e.Service.Tags,
			Address: e.Service.Address,
			Port:    e.Service.Port,
			Meta:    e.Service.Meta,
			Passing: passing,
			Checks:  checks,
		})
	}
	return out, nil
}

func (c *Client) currentToken() (string, error) {
	if c.tokenFile == "" {
		return c.token, nil
	}
	b, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return "", fmt.Errorf("read consul token file: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// escapePath escapes each element of a path, so a name with a question mark or a number sign
// in it stays a name.
func escapePath(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// Put writes a key and its value in Consul KV.
func (c *Client) Put(ctx context.Context, key string, value []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.addr+kvPrefix+escapePath(key), bytes.NewReader(value))
	if err != nil {
		return fmt.Errorf("consul request %s: %w", key, err)
	}
	token, err := c.currentToken()
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set(headerConsulToken, token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("consul PUT kv/%s: %w", key, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("consul PUT kv/%s: %w", key, &StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(data))})
	}
	return nil
}

// Delete removes a key or prefix from Consul KV.
func (c *Client) Delete(ctx context.Context, key string, recurse bool) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u := c.addr + kvPrefix + escapePath(key)
	if recurse {
		u += "?recurse=true"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return fmt.Errorf("consul request %s: %w", key, err)
	}
	token, err := c.currentToken()
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set(headerConsulToken, token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("consul DELETE kv/%s: %w", key, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("consul DELETE kv/%s: %w", key, &StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(data))})
	}
	return nil
}

// PutCAS writes a key and its value in Consul KV if the current ModifyIndex matches cas.
// With cas = 0, it creates the key only if it does not already exist.
func (c *Client) PutCAS(ctx context.Context, key string, value []byte, cas uint64) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u := c.addr + kvPrefix + escapePath(key) + "?cas=" + strconv.FormatUint(cas, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(value))
	if err != nil {
		return fmt.Errorf("consul request %s: %w", key, err)
	}
	token, err := c.currentToken()
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set(headerConsulToken, token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("consul PUT kv/%s: %w", key, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("consul PUT kv/%s: %w", key, &StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(data))})
	}
	if strings.TrimSpace(string(data)) != "true" {
		return fmt.Errorf("consul PUT kv/%s: %w", key, ErrConflict)
	}
	return nil
}
