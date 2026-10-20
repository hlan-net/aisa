package admin

import (
	"bufio"
	"bytes"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
)

// DocItem describes one Markdown document of aisa's documentation.
type DocItem struct {
	Path    string `json:"path"`    // Path below docs/, e.g. "concepts/ARCHITECTURE.md"
	Title   string `json:"title"`   // The document's first level-one heading, or its file name
	Section string `json:"section"` // The directory below docs/, e.g. "concepts"
}

// DocPage is one document rendered as HTML.
type DocPage struct {
	DocItem
	HTML string `json:"html"`
}

// markdown renders GitHub-flavoured Markdown (tables, strikethrough, autolinks, task lists).
// Raw HTML in the documents is left out, as goldmark does without html.WithUnsafe, so the
// rendered page carries no scripts.
var markdown = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
)

// listDocs returns the documents of h.docs, sorted by section and path.
func (h *Handler) listDocs(w http.ResponseWriter, _ *http.Request) {
	items := []DocItem{}
	err := fs.WalkDir(h.docs, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path.Ext(p) != ".md" {
			return nil
		}
		src, err := fs.ReadFile(h.docs, p)
		if err != nil {
			return err
		}
		items = append(items, docItem(p, src))
		return nil
	})
	if err != nil {
		h.log.Error("listing docs", "error", err)
		writeError(w, http.StatusInternalServerError, "cannot list docs")
		return
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Section != items[j].Section {
			return items[i].Section < items[j].Section
		}
		return items[i].Path < items[j].Path
	})
	writeJSON(w, http.StatusOK, items)
}

// getDoc renders the document at {path} as HTML.
func (h *Handler) getDoc(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("path")
	if !fs.ValidPath(p) || path.Ext(p) != ".md" {
		writeError(w, http.StatusNotFound, "document not found")
		return
	}
	src, err := fs.ReadFile(h.docs, p)
	if err != nil {
		writeError(w, http.StatusNotFound, "document not found")
		return
	}
	var buf bytes.Buffer
	if err := markdown.Convert(src, &buf); err != nil {
		h.log.Error("rendering doc", "path", p, "error", err)
		writeError(w, http.StatusInternalServerError, "cannot render document")
		return
	}
	writeJSON(w, http.StatusOK, DocPage{DocItem: docItem(p, src), HTML: buf.String()})
}

// docItem describes the document at p with Markdown source src.
func docItem(p string, src []byte) DocItem {
	item := DocItem{Path: p, Title: strings.TrimSuffix(path.Base(p), ".md")}
	if dir := path.Dir(p); dir != "." {
		item.Section = dir
	}
	sc := bufio.NewScanner(bytes.NewReader(src))
	for sc.Scan() {
		if title, ok := strings.CutPrefix(sc.Text(), "# "); ok {
			item.Title = strings.TrimSpace(title)
			break
		}
	}
	return item
}
