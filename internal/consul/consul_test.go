package consul

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestList(t *testing.T) {
	var gotQuery, gotToken, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotToken = r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("X-Consul-Token")
		w.Header().Set("X-Consul-Index", "42")
		// "YQ==" is "a"; a folder has a null value.
		_, _ = w.Write([]byte(`[{"Key":"aisa/quotas/","Value":null},{"Key":"aisa/quotas/batch","Value":"YQ=="}]`))
	}))
	defer srv.Close()

	c, err := New(Options{Addr: srv.URL, Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	pairs, idx, err := c.List(context.Background(), "aisa/quotas/", 7, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if idx != 42 {
		t.Errorf("index = %d, want 42", idx)
	}
	if len(pairs) != 2 || pairs[1].Key != "aisa/quotas/batch" || string(pairs[1].Value) != "a" || pairs[0].Value != nil {
		t.Errorf("pairs = %+v", pairs)
	}
	if gotPath != "/v1/kv/aisa/quotas/" || gotQuery != "index=7&recurse=true&wait=30s" || gotToken != "tok" {
		t.Errorf("request: path %q, query %q, token %q", gotPath, gotQuery, gotToken)
	}
}

func TestListFirstQueryDoesNotBlock(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("X-Consul-Index", "3")
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c, _ := New(Options{Addr: srv.URL})
	pairs, idx, err := c.List(context.Background(), "aisa/quotas/", 0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if pairs == nil || len(pairs) != 0 || idx != 3 {
		t.Errorf("an empty prefix: pairs %v, index %d; want an empty list and 3", pairs, idx)
	}
	if gotQuery != "recurse=true" {
		t.Errorf("query = %q, want recurse only", gotQuery)
	}
}

func TestListErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"denied", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Consul-Index", "1")
			http.Error(w, "Permission denied", http.StatusForbidden)
		}},
		{"a 404 not from Consul", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}},
		{"a 200 not from Consul", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`[]`))
		}},
		{"a broken index", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Consul-Index", "x")
			_, _ = w.Write([]byte(`[]`))
		}},
		{"not JSON", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Consul-Index", "1")
			_, _ = w.Write([]byte(`<html>`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			c, _ := New(Options{Addr: srv.URL})
			if _, _, err := c.List(context.Background(), "aisa/quotas/", 0, time.Minute); err == nil {
				t.Error("no error")
			}
		})
	}
}

func TestStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Permission denied", http.StatusForbidden)
	}))
	defer srv.Close()
	c, _ := New(Options{Addr: srv.URL})
	_, _, err := c.List(context.Background(), "p/", 0, time.Minute)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusForbidden || se.Body != "Permission denied" {
		t.Errorf("err = %v, want a StatusError 403", err)
	}
}

func TestTokenFileIsReadForEachRequest(t *testing.T) {
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.Header.Get("X-Consul-Token"))
		w.Header().Set("X-Consul-Index", "1")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _ := New(Options{Addr: srv.URL, TokenFile: file})
	if _, _, err := c.List(context.Background(), "p/", 0, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.List(context.Background(), "p/", 0, time.Minute); err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 || tokens[0] != "one" || tokens[1] != "two" {
		t.Errorf("tokens = %q, want one then two", tokens)
	}

	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.List(context.Background(), "p/", 0, time.Minute); err == nil {
		t.Error("a missing token file is no error")
	}
}

func TestNew(t *testing.T) {
	for _, tc := range []struct {
		opts   Options
		want   string
		wantOK bool
	}{
		{Options{Addr: "consul:8500"}, "http://consul:8500", true},
		{Options{Addr: "https://consul.example:8501/"}, "https://consul.example:8501", true},
		{Options{Addr: "ftp://consul"}, "", false},
		{Options{Addr: ""}, "", false},
		{Options{Addr: "consul:8500", Token: "a", TokenFile: "b"}, "", false},
		{Options{Addr: "consul:8500", CACert: "/does/not/exist"}, "", false},
	} {
		c, err := New(tc.opts)
		if (err == nil) != tc.wantOK {
			t.Errorf("New(%+v): err = %v, want ok %v", tc.opts, err, tc.wantOK)
			continue
		}
		if err == nil && c.addr != tc.want {
			t.Errorf("New(%+v): addr = %q, want %q", tc.opts, c.addr, tc.want)
		}
	}
}

func TestPut(t *testing.T) {
	var gotPath, gotBody, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotToken = r.Header.Get("X-Consul-Token")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("true"))
	}))
	defer srv.Close()

	c, _ := New(Options{Addr: srv.URL, Token: "my-tok"})
	err := c.Put(context.Background(), "aisa/consumers/app1", []byte(`{"quota_profile":"interactive"}`))
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if gotPath != "/v1/kv/aisa/consumers/app1" {
		t.Errorf("path = %q, want /v1/kv/aisa/consumers/app1", gotPath)
	}
	if gotToken != "my-tok" {
		t.Errorf("token = %q, want my-tok", gotToken)
	}
	if gotBody != `{"quota_profile":"interactive"}` {
		t.Errorf("body = %q", gotBody)
	}

	// Error handling
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer errSrv.Close()
	cErr, _ := New(Options{Addr: errSrv.URL})
	if err := cErr.Put(context.Background(), "key", []byte("val")); err == nil {
		t.Error("expected error on 500, got nil")
	}
}

func TestDelete(t *testing.T) {
	var gotPath, gotQuery, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.EscapedPath()
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("true"))
	}))
	defer srv.Close()

	c, _ := New(Options{Addr: srv.URL})
	if err := c.Delete(context.Background(), "aisa/consumers/app1", false); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v1/kv/aisa/consumers/app1" || gotQuery != "" {
		t.Errorf("Delete: method %s, path %s, query %s", gotMethod, gotPath, gotQuery)
	}

	if err := c.Delete(context.Background(), "aisa/consumers/", true); err != nil {
		t.Fatalf("Delete recurse failed: %v", err)
	}
	if gotQuery != "recurse=true" {
		t.Errorf("Delete recurse query = %q, want recurse=true", gotQuery)
	}
}

func TestHealthService(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[
			{
				"Service": {
					"ID": "backend-1",
					"Service": "aisa-backend",
					"Tags": ["mock"],
					"Address": "127.0.0.1",
					"Port": 8080,
					"Meta": {"models": "qwen3", "provider": "openai-compatible"}
				},
				"Checks": [
					{
						"CheckID": "service:backend-1",
						"Name": "backend-1 health",
						"Status": "passing",
						"Output": "HTTP 200"
					}
				]
			}
		]`))
	}))
	defer srv.Close()

	c, _ := New(Options{Addr: srv.URL})
	list, err := c.HealthService(context.Background(), "aisa-backend", true)
	if err != nil {
		t.Fatalf("HealthService failed: %v", err)
	}
	if gotPath != "/v1/health/service/aisa-backend" || gotQuery != "passing=true" {
		t.Errorf("request: path %q, query %q", gotPath, gotQuery)
	}
	if len(list) != 1 || list[0].Service.ID != "backend-1" || list[0].Service.Meta["models"] != "qwen3" {
		t.Errorf("unexpected list: %+v", list)
	}
	if len(list[0].Checks) != 1 || list[0].Checks[0].Status != "passing" {
		t.Errorf("unexpected checks: %+v", list[0].Checks)
	}
}

func TestDeleteErrorsAndToken(t *testing.T) {
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Consul-Token")
		if r.URL.Path == "/v1/kv/fail" {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("true"))
	}))
	defer srv.Close()

	c, _ := New(Options{Addr: srv.URL, Token: "test-token"})
	if err := c.Delete(context.Background(), "fail", false); err == nil {
		t.Error("expected error on 500, got nil")
	}
	if err := c.Delete(context.Background(), "ok", false); err != nil {
		t.Fatalf("delete ok failed: %v", err)
	}
	if gotToken != "test-token" {
		t.Errorf("got token %q, want test-token", gotToken)
	}
}

func TestHealthServiceErrorsAndToken(t *testing.T) {
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Consul-Token")
		if r.URL.Path == "/v1/health/service/fail" {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		if r.URL.Path == "/v1/health/service/bad-json" {
			_, _ = w.Write([]byte("not json"))
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	c, _ := New(Options{Addr: srv.URL, Token: "test-token"})
	if _, err := c.HealthService(context.Background(), "fail", false); err == nil {
		t.Error("expected error on 500, got nil")
	}
	if _, err := c.HealthService(context.Background(), "bad-json", false); err == nil {
		t.Error("expected error on bad json, got nil")
	}
	if _, err := c.HealthService(context.Background(), "ok", false); err != nil {
		t.Fatalf("healthservice ok failed: %v", err)
	}
	if gotToken != "test-token" {
		t.Errorf("got token %q, want test-token", gotToken)
	}
}
