package admin

import (
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

func TestDocIndexRendersAndRewritesLinks(t *testing.T) {
	fsys := fstest.MapFS{
		"concepts/VAULT.md": {Data: []byte("# Vault\n\n## S7 + S10: Footprint\n\n## Exposure\n\n## Exposure\n\n## Exposure-1\n\n## Exposure\n")},
		"features/quotas.md": {Data: []byte(strings.Join([]string{
			"# Quotas and *budgets*",
			"",
			"[same section](#exposure)",
			"[other doc](../concepts/VAULT.md#s7--s10-footprint)",
			"[roadmap](../../ROADMAP.md)",
			"[outside](../../../elsewhere.md)",
			"[web](https://example.com/x.md)",
			"",
			"| a | b |",
			"|---|---|",
			"| 1 | 2 |",
			"",
			"<script>alert(1)</script>",
		}, "\n"))},
	}
	idx, err := newDocIndex(fsys)
	if err != nil {
		t.Fatalf("newDocIndex: %v", err)
	}

	if len(idx.items) != 2 || idx.items[0].Path != "concepts/VAULT.md" || idx.items[1].Section != "features" {
		t.Fatalf("items = %+v", idx.items)
	}
	if got := idx.items[1].Title; got != "Quotas and budgets" {
		t.Errorf("title = %q", got)
	}

	vault := idx.pages["concepts/VAULT.md"].HTML
	for _, want := range []string{`id="s7--s10-footprint"`, `id="exposure"`, `id="exposure-1"`, `id="exposure-1-1"`, `id="exposure-2"`} {
		if !strings.Contains(vault, want) {
			t.Errorf("VAULT.md HTML lacks %s:\n%s", want, vault)
		}
	}

	quotas := idx.pages["features/quotas.md"].HTML
	for _, want := range []string{
		`href="#exposure"`,
		`href="?doc=concepts/VAULT.md#s7--s10-footprint"`,
		`href="` + repoBlobURL + `ROADMAP.md"`,
		`href="../../../elsewhere.md"`,
		`href="https://example.com/x.md"`,
		`<table>`,
	} {
		if !strings.Contains(quotas, want) {
			t.Errorf("quotas.md HTML lacks %s:\n%s", want, quotas)
		}
	}
	if strings.Contains(quotas, "<script>") {
		t.Errorf("raw HTML was rendered:\n%s", quotas)
	}
}

func TestDocsEndpoints(t *testing.T) {
	h, _, _ := setupTestHandler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/api/docs", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d", w.Code)
	}
	var items []DocItem
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	found := false
	for _, it := range items {
		if it.Path == "concepts/ARCHITECTURE.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("embedded docs lack concepts/ARCHITECTURE.md: %+v", items)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/api/docs/concepts/ARCHITECTURE.md", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d", w.Code)
	}
	var page DocPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if page.Title == "" || !strings.Contains(page.HTML, "<h1") {
		t.Errorf("page = %+v", page.DocItem)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/api/docs/../go.mod", nil))
	if w.Code == http.StatusOK {
		t.Errorf("a path outside the docs answered 200")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/api/docs/concepts/NOPE.md", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("missing doc status = %d, want 404", w.Code)
	}
}

// TestEmbeddedDocLinksResolve checks that every link from one embedded document to another, and to
// a heading in it, lands on something the admin UI can show.
func TestEmbeddedDocLinksResolve(t *testing.T) {
	h, _, _ := setupTestHandler()
	if h.docs == nil {
		t.Fatal("embedded documentation was not indexed")
	}
	hrefRe := regexp.MustCompile(`href="([^"]*)"`)
	for p, page := range h.docs.pages {
		for _, m := range hrefRe.FindAllStringSubmatch(page.HTML, -1) {
			href := html.UnescapeString(m[1])
			target, frag := p, ""
			switch {
			case strings.HasPrefix(href, "?doc="):
				target, frag, _ = strings.Cut(strings.TrimPrefix(href, "?doc="), "#")
			case strings.HasPrefix(href, "#"):
				frag = href[1:]
			default:
				continue
			}
			tp, ok := h.docs.pages[target]
			if !ok {
				t.Errorf("%s: link %q to a document that is not embedded", p, href)
				continue
			}
			if frag != "" && !strings.Contains(tp.HTML, `id="`+frag+`"`) {
				t.Errorf("%s: link %q to a heading %s does not have", p, href, target)
			}
		}
	}
}
