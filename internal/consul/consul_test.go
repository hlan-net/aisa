package consul

import (
	"context"
	"errors"
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
