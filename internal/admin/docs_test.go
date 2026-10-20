package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hlan-net/aisa/internal/metrics"
)

func newDocsHandler(docs fstest.MapFS) *Handler {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return New(newMockConsul(), newMockVault(), metrics.New("test"), log, Options{Docs: docs})
}

func TestListDocs(t *testing.T) {
	h := newDocsHandler(fstest.MapFS{
		"features/usage.md":        {Data: []byte("# Usage metrics\n\ntext\n")},
		"concepts/ARCHITECTURE.md": {Data: []byte("Intro\n\n# Architecture\n")},
		"concepts/NOTITLE.md":      {Data: []byte("no heading\n")},
		"concepts/docs.go":         {Data: []byte("package docs\n")},
	})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/api/docs", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var items []DocItem
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	want := []DocItem{
		{Path: "concepts/ARCHITECTURE.md", Title: "Architecture", Section: "concepts"},
		{Path: "concepts/NOTITLE.md", Title: "NOTITLE", Section: "concepts"},
		{Path: "features/usage.md", Title: "Usage metrics", Section: "features"},
	}
	if len(items) != len(want) {
		t.Fatalf("items = %+v, want %+v", items, want)
	}
	for i := range want {
		if items[i] != want[i] {
			t.Errorf("items[%d] = %+v, want %+v", i, items[i], want[i])
		}
	}
}

func TestGetDoc(t *testing.T) {
	h := newDocsHandler(fstest.MapFS{
		"concepts/ARCHITECTURE.md": {Data: []byte("# Architecture\n\n## What aisa is\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n<script>alert(1)</script>\n\n[Vault](VAULT.md#keys)\n")},
	})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/api/docs/concepts/ARCHITECTURE.md", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var page DocPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if page.Title != "Architecture" || page.Path != "concepts/ARCHITECTURE.md" {
		t.Errorf("page = %+v", page.DocItem)
	}
	for _, s := range []string{`<h2 id="what-aisa-is">`, "<table>", `<a href="VAULT.md#keys">`} {
		if !strings.Contains(page.HTML, s) {
			t.Errorf("html misses %q:\n%s", s, page.HTML)
		}
	}
	if strings.Contains(page.HTML, "<script>") {
		t.Errorf("html carries raw HTML from the document:\n%s", page.HTML)
	}
}

func TestGetDocNotFound(t *testing.T) {
	h := newDocsHandler(fstest.MapFS{
		"concepts/ARCHITECTURE.md": {Data: []byte("# Architecture\n")},
		"concepts/docs.go":         {Data: []byte("package docs\n")},
	})
	for _, p := range []string{"concepts/MISSING.md", "concepts/docs.go", "concepts"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/api/docs/"+p, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", p, w.Code)
		}
	}
}

// TestEmbeddedDocs checks that the documentation of the repository is in the binary and renders.
func TestEmbeddedDocs(t *testing.T) {
	h, _, _ := setupTestHandler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/api/docs", nil))
	var items []DocItem
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("no embedded documents")
	}
	for _, item := range items {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/api/docs/"+item.Path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", item.Path, w.Code)
		}
	}
}
