package admin

import (
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hlan-net/aisa"
	"github.com/hlan-net/aisa/internal/metrics"
)

var testDocs = fstest.MapFS{
	"README.md": {Data: []byte("# aisa\n\nSee [the architecture](docs/concepts/ARCHITECTURE.md#what-aisa-is), " +
		"[the chart](deploy/helm/aisa/values.yaml), [Go](https://go.dev) and [below](#s7--s10-footprint).\n\n" +
		"## S7 / S10: footprint\n\n<script>alert(1)</script>\n\n| a | b |\n|---|---|\n| 1 | 2 |\n")},
	"ROADMAP.md":                      {Data: []byte("# Roadmap\n")},
	"docs/concepts/ARCHITECTURE.md":   {Data: []byte("# Architecture\n\n## What aisa is\n\nBack to [the contract](./PROXY_CONTRACT.md) and [features](../features/usage-metrics.md).\n")},
	"docs/concepts/PROXY_CONTRACT.md": {Data: []byte("No heading here.\n")},
	"docs/features/usage-metrics.md":  {Data: []byte("# Usage metrics\n")},
	"docs/features/notes.txt":         {Data: []byte("not a document")},
}

func docsHandler(t *testing.T, fsys fs.FS) *Handler {
	t.Helper()
	return New(newMockConsul(), newMockVault(), metrics.New("v0.2.0-test"),
		slog.New(slog.NewJSONHandler(io.Discard, nil)), Options{Docs: fsys})
}

func getJSON(t *testing.T, h http.Handler, target string, v any) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
			t.Fatalf("decode %s: %v", target, err)
		}
	}
	return rec.Code
}

func TestDocsList(t *testing.T) {
	h := docsHandler(t, testDocs)
	var got struct{ Docs []docInfo }
	if code := getJSON(t, h, "/admin/api/docs", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	want := []docInfo{
		{Path: "README.md", Title: "aisa"},
		{Path: "ROADMAP.md", Title: "Roadmap"},
		{Path: "docs/concepts/ARCHITECTURE.md", Title: "Architecture", Section: "concepts"},
		{Path: "docs/concepts/PROXY_CONTRACT.md", Title: "PROXY_CONTRACT.md", Section: "concepts"},
		{Path: "docs/features/usage-metrics.md", Title: "Usage metrics", Section: "features"},
	}
	if len(got.Docs) != len(want) {
		t.Fatalf("docs = %+v, want %+v", got.Docs, want)
	}
	for i := range want {
		if got.Docs[i] != want[i] {
			t.Errorf("docs[%d] = %+v, want %+v", i, got.Docs[i], want[i])
		}
	}
}

func TestDocRendered(t *testing.T) {
	h := docsHandler(t, testDocs)
	var got struct{ Path, Title, HTML string }
	if code := getJSON(t, h, "/admin/api/docs/README.md", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	for _, want := range []string{
		`<h1 id="doc-aisa">aisa</h1>`,
		`<h2 id="doc-s7--s10-footprint">`,
		`href="#docs/docs/concepts/ARCHITECTURE.md#what-aisa-is"`,
		`href="https://github.com/hlan-net/aisa/blob/main/deploy/helm/aisa/values.yaml" target="_blank" rel="noopener noreferrer"`,
		`href="https://go.dev" target="_blank" rel="noopener noreferrer"`,
		`href="#docs/README.md#s7--s10-footprint"`,
		`<table>`,
	} {
		if !strings.Contains(got.HTML, want) {
			t.Errorf("html lacks %s:\n%s", want, got.HTML)
		}
	}
	if strings.Contains(got.HTML, "<script>") {
		t.Errorf("raw HTML was rendered:\n%s", got.HTML)
	}
	if got.Title != "aisa" || got.Path != "README.md" {
		t.Errorf("path, title = %q, %q", got.Path, got.Title)
	}

	if code := getJSON(t, h, "/admin/api/docs/docs/concepts/ARCHITECTURE.md", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	for _, want := range []string{
		`href="#docs/docs/concepts/PROXY_CONTRACT.md"`,
		`href="#docs/docs/features/usage-metrics.md"`,
	} {
		if !strings.Contains(got.HTML, want) {
			t.Errorf("html lacks %s:\n%s", want, got.HTML)
		}
	}
}

func TestDocNotFound(t *testing.T) {
	h := docsHandler(t, testDocs)
	for _, p := range []string{"docs/features/notes.txt", "../go.mod", "docs/../README.md", "missing.md"} {
		// The mux answers the paths with ".." with a redirect to the cleaned path.
		if code := getJSON(t, h, "/admin/api/docs/"+p, &struct{}{}); code == http.StatusOK {
			t.Errorf("%s: status = %d", p, code)
		}
	}
}

func TestDocsNone(t *testing.T) {
	h := docsHandler(t, nil)
	var got struct{ Docs []docInfo }
	if code := getJSON(t, h, "/admin/api/docs", &got); code != http.StatusOK || len(got.Docs) != 0 {
		t.Fatalf("status = %d, docs = %+v", code, got.Docs)
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"What aisa is":                                "what-aisa-is",
		"S7 / S10: footprint and latency":             "s7--s10-footprint-and-latency",
		"3. Config rendering (gateway configuration)": "3-config-rendering-gateway-configuration",
		"The consumer's Secret":                       "the-consumers-secret",
		"Backends (Consul catalog)":                   "backends-consul-catalog",
	} {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRepositoryDocLinks renders every document of the repository and checks that each link
// to a document and its heading finds them.
func TestRepositoryDocLinks(t *testing.T) {
	d, err := newDocs(aisa.Docs)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.list) < 10 {
		t.Fatalf("only %d documents embedded", len(d.list))
	}
	ids := map[string]map[string]bool{}
	html := map[string]string{}
	idRe := regexp.MustCompile(`id="` + headingIDPrefix + `([^"]+)"`)
	for _, doc := range d.list {
		out, err := d.render(doc.Path)
		if err != nil {
			t.Fatalf("render %s: %v", doc.Path, err)
		}
		html[doc.Path] = out
		ids[doc.Path] = map[string]bool{}
		for _, m := range idRe.FindAllStringSubmatch(out, -1) {
			ids[doc.Path][m[1]] = true
		}
	}
	linkRe := regexp.MustCompile(`href="#docs/([^"#]+)(?:#([^"]*))?"`)
	for p, out := range html {
		for _, m := range linkRe.FindAllStringSubmatch(out, -1) {
			target, anchor := m[1], m[2]
			if !d.known[target] {
				t.Errorf("%s links to unknown document %s", p, target)
				continue
			}
			if anchor != "" && !ids[target][anchor] {
				t.Errorf("%s links to #%s, which %s does not have", p, anchor, target)
			}
		}
	}
}
