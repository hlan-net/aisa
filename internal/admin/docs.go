package admin

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
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
	"github.com/yuin/goldmark/util"

	"github.com/hlan-net/aisa/internal/version"
)

// repoURL is where links to files of the repository that are not documents point.
const repoURL = "https://github.com/hlan-net/aisa"

// headingIDPrefix keeps the ids of a document's headings apart from the ids of the dashboard.
const headingIDPrefix = "doc-"

// docsHash starts the dashboard's link to a document: #docs/<path>[#<heading>].
const docsHash = "#docs/"

// rootDocs are the documents at the top of the repository, in the order the UI lists them.
var rootDocs = []string{"README.md", "ROADMAP.md", "CHANGELOG.md"}

// docInfo describes one document in the list of documents.
type docInfo struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Section string `json:"section"`
}

// docs serves the Markdown documents of a file system, rendered as HTML.
type docs struct {
	fsys  fs.FS
	list  []docInfo
	known map[string]bool
	md    goldmark.Markdown
}

// newDocs indexes the Markdown files of fsys: the root documents and everything under docs/.
func newDocs(fsys fs.FS) (*docs, error) {
	d := &docs{fsys: fsys, known: map[string]bool{}}
	if fsys == nil {
		return d, nil
	}
	for _, p := range rootDocs {
		if _, err := fs.Stat(fsys, p); err == nil {
			d.add(p, "")
		}
	}
	var nested []string
	err := fs.WalkDir(fsys, "docs", func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.IsDir() && strings.HasSuffix(p, ".md") {
			nested = append(nested, p)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("index documents: %w", err)
	}
	sort.Strings(nested)
	for _, p := range nested {
		d.add(p, path.Base(path.Dir(p)))
	}
	// Raw HTML in a document is left out: goldmark renders it only WithUnsafe.
	d.md = goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID(), parser.WithASTTransformers(
			util.Prioritized(&linkRewriter{known: d.known}, 100))))
	return d, nil
}

func (d *docs) add(p, section string) {
	d.list = append(d.list, docInfo{Path: p, Title: d.title(p), Section: section})
	d.known[p] = true
}

// title is the document's first level-1 heading, or its file name.
func (d *docs) title(p string) string {
	b, err := fs.ReadFile(d.fsys, p)
	if err != nil {
		return path.Base(p)
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if t, ok := strings.CutPrefix(sc.Text(), "# "); ok {
			return strings.TrimSpace(t)
		}
	}
	return path.Base(p)
}

// render returns the document at p as HTML. Raw HTML in the document is left out.
func (d *docs) render(p string) (string, error) {
	src, err := fs.ReadFile(d.fsys, p)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", p, err)
	}
	ctx := parser.NewContext(parser.WithIDs(&headingIDs{seen: map[string]int{}}))
	ctx.Set(currentDocKey, p)
	var buf bytes.Buffer
	if err := d.md.Convert(src, &buf, parser.WithContext(ctx)); err != nil {
		return "", fmt.Errorf("render %s: %w", p, err)
	}
	return buf.String(), nil
}

func (h *Handler) listDocs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"docs": h.docs.list})
}

func (h *Handler) getDoc(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("path")
	if !h.docs.known[p] {
		writeError(w, http.StatusNotFound, "no such document")
		return
	}
	html, err := h.docs.render(p)
	if err != nil {
		h.log.Error("render document", "path", p, "err", err)
		writeError(w, http.StatusInternalServerError, "document could not be rendered")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": p, "title": h.docs.title(p), "html": html})
}

// headingIDs gives headings the ids GitHub gives them, so links written for GitHub find their
// heading here too, behind headingIDPrefix.
type headingIDs struct {
	seen map[string]int
}

func (h *headingIDs) Generate(value []byte, _ ast.NodeKind) []byte {
	slug := slugify(string(value))
	n := h.seen[slug]
	h.seen[slug] = n + 1
	if n > 0 {
		slug += "-" + strconv.Itoa(n)
	}
	return []byte(headingIDPrefix + slug)
}

func (h *headingIDs) Put(value []byte) {
	h.seen[string(value)]++
}

// slugify makes a GitHub heading anchor: lower case, spaces as hyphens, and only letters,
// digits, hyphens and underscores kept.
func slugify(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// currentDocKey holds the path of the document being rendered in a parser.Context.
var currentDocKey = parser.NewContextKey()

// linkRewriter points links between documents at the dashboard's document view, and links to
// other files of the repository at the repository.
type linkRewriter struct {
	known map[string]bool
}

func (t *linkRewriter) Transform(doc *ast.Document, _ text.Reader, pc parser.Context) {
	current, _ := pc.Get(currentDocKey).(string)
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		l, ok := n.(*ast.Link)
		if !entering || !ok {
			return ast.WalkContinue, nil
		}
		dest, external := t.rewrite(current, string(l.Destination))
		l.Destination = []byte(dest)
		if external {
			l.SetAttributeString("target", []byte("_blank"))
			l.SetAttributeString("rel", []byte("noopener noreferrer"))
		}
		return ast.WalkContinue, nil
	})
}

// rewrite returns the new destination of a link in the document current and whether it leaves
// the dashboard.
func (t *linkRewriter) rewrite(current, dest string) (string, bool) {
	if dest == "" {
		return dest, false
	}
	if strings.HasPrefix(dest, "#") {
		return docsHash + current + dest, false
	}
	if strings.Contains(dest, "://") || strings.HasPrefix(dest, "mailto:") || strings.HasPrefix(dest, "/") {
		return dest, true
	}
	target, frag, _ := strings.Cut(dest, "#")
	resolved := path.Clean(path.Join(path.Dir(current), target))
	if t.known[resolved] {
		if frag != "" {
			return docsHash + resolved + "#" + frag, false
		}
		return docsHash + resolved, false
	}
	if strings.HasPrefix(resolved, "../") || resolved == ".." {
		return dest, true
	}
	ref := "main"
	if strings.HasPrefix(version.Version, "v") {
		ref = version.Version
	}
	u := repoURL + "/blob/" + ref + "/" + resolved
	if frag != "" {
		u += "#" + frag
	}
	return u, true
}
