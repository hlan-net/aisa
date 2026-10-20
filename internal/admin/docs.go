package admin

import (
	"bytes"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// repoBlobURL is where links from the documentation to files outside docs/ point, since only the
// Markdown under docs/ is in the binary.
const repoBlobURL = "https://github.com/hlan-net/aisa/blob/main/"

// sectionOrder is the order of docs/ subdirectories in the documentation index.
var sectionOrder = map[string]int{"concepts": 0, "features": 1, "process": 2}

// DocItem describes one document in the documentation index.
type DocItem struct {
	Path    string `json:"path"`
	Section string `json:"section"`
	Title   string `json:"title"`
}

// DocPage is one document rendered to HTML.
type DocPage struct {
	DocItem
	HTML string `json:"html"`
}

// docIndex holds the documentation rendered once at start-up; the files are embedded, so they
// cannot change while aisa runs.
type docIndex struct {
	items []DocItem
	pages map[string]DocPage
}

// newDocIndex renders every Markdown file of fsys. Links between documents are rewritten to the
// admin UI's own document view (?doc=<path>), and links to other files of the repository to the
// repository on GitHub.
func newDocIndex(fsys fs.FS) (*docIndex, error) {
	var paths []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".md") {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list documentation: %w", err)
	}
	known := make(map[string]bool, len(paths))
	for _, p := range paths {
		known[p] = true
	}

	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	)
	idx := &docIndex{pages: make(map[string]DocPage, len(paths))}
	for _, p := range paths {
		src, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		page, err := renderDoc(md, p, src, known)
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", p, err)
		}
		idx.items = append(idx.items, page.DocItem)
		idx.pages[p] = page
	}
	sort.Slice(idx.items, func(i, j int) bool {
		a, b := idx.items[i], idx.items[j]
		if a.Section != b.Section {
			return sectionRank(a.Section) < sectionRank(b.Section)
		}
		return a.Path < b.Path
	})
	return idx, nil
}

func sectionRank(s string) int {
	if r, ok := sectionOrder[s]; ok {
		return r
	}
	return len(sectionOrder)
}

func renderDoc(md goldmark.Markdown, p string, src []byte, known map[string]bool) (DocPage, error) {
	ctx := parser.NewContext(parser.WithIDs(newGitHubIDs()))
	doc := md.Parser().Parse(text.NewReader(src), parser.WithContext(ctx))

	page := DocPage{DocItem: DocItem{Path: p, Section: path.Dir(p)}}
	if page.Section == "." {
		page.Section = ""
	}
	err := ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Heading:
			if n.Level == 1 && page.Title == "" {
				page.Title = plainText(n, src)
			}
		case *ast.Link:
			n.Destination = []byte(rewriteLink(p, string(n.Destination), known))
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		return DocPage{}, fmt.Errorf("walk: %w", err)
	}
	if page.Title == "" {
		page.Title = path.Base(p)
	}

	var buf bytes.Buffer
	if err := md.Renderer().Render(&buf, src, doc); err != nil {
		return DocPage{}, fmt.Errorf("html: %w", err)
	}
	page.HTML = buf.String()
	return page, nil
}

// rewriteLink maps a link in document from to where the admin UI can follow it.
func rewriteLink(from, dest string, known map[string]bool) string {
	u, err := url.Parse(dest)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Path == "" || strings.HasPrefix(u.Path, "/") {
		return dest
	}
	frag := ""
	if u.Fragment != "" {
		frag = "#" + u.Fragment
	}
	target := path.Join(path.Dir(from), u.Path)
	if known[target] {
		return "?doc=" + target + frag
	}
	repoPath := path.Join("docs", target)
	if strings.HasPrefix(repoPath, "../") {
		return dest
	}
	return repoBlobURL + repoPath + frag
}

// plainText returns the text of n's inline children, without markup.
func plainText(n ast.Node, src []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch c := c.(type) {
		case *ast.Text:
			b.Write(c.Segment.Value(src))
		case *ast.String:
			b.Write(c.Value)
		}
		return ast.WalkContinue, nil
	})
	return strings.TrimSpace(b.String())
}

// gitHubIDs gives headings the anchors GitHub gives them, so links such as
// ./CONSUL.md#backends-consul-catalog work in the admin UI as they do on GitHub.
type gitHubIDs struct {
	seen map[string]int
}

func newGitHubIDs() *gitHubIDs {
	return &gitHubIDs{seen: map[string]int{}}
}

func (g *gitHubIDs) Generate(value []byte, _ ast.NodeKind) []byte {
	var b strings.Builder
	for _, r := range strings.ToLower(string(value)) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	id := b.String()
	if id == "" {
		id = "section"
	}
	n := g.seen[id]
	g.seen[id] = n + 1
	if n > 0 {
		id += "-" + strconv.Itoa(n)
	}
	return []byte(id)
}

func (g *gitHubIDs) Put(value []byte) {
	g.seen[string(value)]++
}

// listDocs serves the documentation index.
func (h *Handler) listDocs(w http.ResponseWriter, _ *http.Request) {
	if h.docs == nil {
		writeJSON(w, http.StatusOK, []DocItem{})
		return
	}
	writeJSON(w, http.StatusOK, h.docs.items)
}

// getDoc serves one document rendered to HTML.
func (h *Handler) getDoc(w http.ResponseWriter, r *http.Request) {
	if h.docs == nil {
		writeError(w, http.StatusNotFound, "document not found")
		return
	}
	page, ok := h.docs.pages[r.PathValue("path")]
	if !ok {
		writeError(w, http.StatusNotFound, "document not found")
		return
	}
	writeJSON(w, http.StatusOK, page)
}
