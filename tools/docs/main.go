// Command docs turns the Markdown manual in docs/manual/ into static HTML for
// the project site (site/docs/) and into one single-page HTML file that Chrome
// prints to PDF (site/docs/_manual.html).
//
// It lives in its own Go module so that the product does not get a dependency
// it never uses. Run it through "make docs" from the repository root.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"html"
	"html/template"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	ghtml "github.com/yuin/goldmark/renderer/html"
)

const (
	siteURL = "https://pgcellar.dbfactory.io"
	tagline = "The barman serves, the cellar keeps"
)

// Images the manual refers to. A grey placeholder is written for each one that
// is missing, so the build always passes; drop the real screenshot in its place.
var screenshots = []string{
	"status", "server-overview", "timeline", "setup", "drills", "restore-run", "runs", "login", "alerts",
}

type tocItem struct {
	Level int
	ID    string
	Text  string
}

type chapter struct {
	N     int
	Slug  string
	File  string
	Title string
	Desc  string
	Words int
	Body  string // rendered HTML, links and ids already fixed for the page mode
	TOC   []tocItem
	md    string
}

type manual struct {
	Title    string
	IntroMD  string
	Chapters []*chapter
	Version  string
	StyleV   string // fingerprint of site/style.css
	DocsV    string // fingerprint of site/docs/docs.css
}

var chapterLine = regexp.MustCompile(`^\s*(\d+)\.\s+\[([^\]]+)\]\(([^)]+\.md)\)\s*(?:[-\x{2013}\x{2014}]\s*(.*))?$`)

func main() {
	root := flag.String("root", ".", "repository root")
	version := flag.String("version", "", "version to print (default: from git tags or CHANGELOG.md)")
	flag.Parse()
	if err := run(*root, *version); err != nil {
		fmt.Fprintln(os.Stderr, "docs:", err)
		os.Exit(1)
	}
}

func run(root, version string) error {
	srcDir := filepath.Join(root, "docs", "manual")
	outDir := filepath.Join(root, "site", "docs")

	m, err := load(srcDir)
	if err != nil {
		return err
	}
	if version == "" {
		version = detectVersion(root)
	}
	m.Version = version
	if m.StyleV, err = fingerprint(filepath.Join(root, "site", "style.css")); err != nil {
		return err
	}
	if m.DocsV, err = fingerprint(filepath.Join(outDir, "docs.css")); err != nil {
		return err
	}

	if err := ensureImages(filepath.Join(srcDir, "images")); err != nil {
		return err
	}
	if err := os.RemoveAll(outDir + "/images"); err != nil {
		return err
	}
	if err := copyDir(filepath.Join(srcDir, "images"), filepath.Join(outDir, "images")); err != nil {
		return err
	}

	// One HTML page per chapter.
	for i, c := range m.Chapters {
		body, toc := render(c.md, modePage, c, m)
		c.Body, c.TOC = body, toc
		if err := writePage(m, i, outDir); err != nil {
			return err
		}
	}
	if err := writeLanding(m, outDir); err != nil {
		return err
	}
	// The single page for the PDF.
	for _, c := range m.Chapters {
		c.Body, c.TOC = render(c.md, modeSingle, c, m)
	}
	if err := writeSingle(m, filepath.Join(outDir, "_manual.html")); err != nil {
		return err
	}
	if err := stampHomePages(root, m.StyleV); err != nil {
		return err
	}
	if err := updateSitemap(filepath.Join(root, "site", "sitemap.xml"), m); err != nil {
		return err
	}
	total := 0
	for _, c := range m.Chapters {
		fmt.Printf("%2d  %-40s %5d words\n", c.N, c.Title, c.Words)
		total += c.Words
	}
	fmt.Printf("    %-40s %5d words\nsite/docs written, version %s\n", "total", total, m.Version)
	return nil
}

// load reads index.md and every chapter it lists, in that order.
func load(dir string) (*manual, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "index.md"))
	if err != nil {
		return nil, err
	}
	m := &manual{}
	var intro []string
	inIntro := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "# ") && m.Title == "" {
			m.Title = strings.TrimSpace(line[2:])
			inIntro = true
			continue
		}
		if strings.HasPrefix(line, "## ") {
			inIntro = false
		}
		if inIntro {
			intro = append(intro, line)
		}
		if mm := chapterLine.FindStringSubmatch(line); mm != nil {
			file := mm[3]
			b, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil {
				return nil, fmt.Errorf("index.md lists %s: %w", file, err)
			}
			c := &chapter{N: len(m.Chapters) + 1, File: file, Title: mm[2], Desc: strings.TrimSpace(mm[4]), md: string(b)}
			c.Slug = strings.TrimSuffix(strings.TrimLeft(file, "0123456789-"), ".md")
			c.Words = len(strings.Fields(c.md))
			m.Chapters = append(m.Chapters, c)
		}
	}
	if m.Title == "" || len(m.Chapters) == 0 {
		return nil, fmt.Errorf("index.md needs a '# title' and a numbered list of chapters")
	}
	m.IntroMD = strings.TrimSpace(strings.Join(intro, "\n"))
	return m, nil
}

// detectVersion uses the latest git tag when there is one, else the newest
// versioned heading of CHANGELOG.md.
func detectVersion(root string) string {
	cmd := exec.Command("git", "describe", "--tags", "--abbrev=0")
	cmd.Dir = root
	if out, err := cmd.Output(); err == nil {
		if v := strings.TrimPrefix(strings.TrimSpace(string(out)), "v"); v != "" {
			return v
		}
	}
	b, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err == nil {
		re := regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\](.*)$`)
		if mm := re.FindSubmatch(b); mm != nil {
			v := string(mm[1])
			if strings.Contains(string(mm[2]), "not released") {
				v += " (not released yet)"
			}
			return v
		}
	}
	return "dev"
}

const (
	modePage   = "page"
	modeSingle = "single"
	modeLand   = "land"
)

var md = goldmark.New(
	goldmark.WithExtensions(extension.Table),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(ghtml.WithXHTML()),
)

var (
	reHead   = regexp.MustCompile(`(?s)<h([1-6]) id="([^"]*)">(.*?)</h[1-6]>`)
	reHref   = regexp.MustCompile(`href="([^"]*)"`)
	reImg    = regexp.MustCompile(`<p><img src="([^"]+)" alt="([^"]*)" ?/?></p>`)
	reTag    = regexp.MustCompile(`<[^>]+>`)
	reSrcImg = regexp.MustCompile(`src="images/`)
)

// render turns Markdown into HTML and fixes ids, links and image paths for the
// page mode: one page per chapter (modePage), the landing page (modeLand) or
// the single page (modeSingle).
func render(src, mode string, c *chapter, m *manual) (string, []tocItem) {
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		return "<p>render error: " + html.EscapeString(err.Error()) + "</p>", nil
	}
	out := buf.String()
	files := map[string]*chapter{}
	for _, ch := range m.Chapters {
		files[ch.File] = ch
	}
	slug := ""
	if c != nil {
		slug = c.Slug
	}

	var toc []tocItem
	out = reHead.ReplaceAllStringFunc(out, func(s string) string {
		mm := reHead.FindStringSubmatch(s)
		level, id, inner := mm[1], mm[2], mm[3]
		text := html.UnescapeString(reTag.ReplaceAllString(inner, ""))
		newID := id
		if mode == modeSingle {
			newID = slug + "-" + id
			if level == "1" {
				newID = slug
			}
		}
		if level == "2" || level == "3" {
			toc = append(toc, tocItem{Level: int(level[0] - '0'), ID: newID, Text: text})
		}
		return fmt.Sprintf(`<h%s id="%s">%s</h%s>`, level, newID, inner, level)
	})

	out = reHref.ReplaceAllStringFunc(out, func(s string) string {
		h := reHref.FindStringSubmatch(s)[1]
		file, frag, _ := strings.Cut(h, "#")
		switch {
		case strings.HasPrefix(h, "#"):
			if mode == modeSingle {
				return `href="#` + slug + "-" + h[1:] + `"`
			}
		case file == "index.md":
			if mode == modeSingle {
				return `href="#top"`
			}
			return `href="` + docsRoot(mode) + `"`
		case strings.HasSuffix(file, ".md"):
			t, ok := files[file]
			if !ok {
				return s
			}
			if mode == modeSingle {
				if frag != "" {
					return `href="#` + t.Slug + "-" + frag + `"`
				}
				return `href="#` + t.Slug + `"`
			}
			link := docsRoot(mode) + t.Slug + "/"
			if frag != "" {
				link += "#" + frag
			}
			return `href="` + link + `"`
		}
		return s
	})

	imgBase := "images/"
	if mode == modePage {
		imgBase = "../images/"
	}
	out = reSrcImg.ReplaceAllString(out, `src="`+imgBase)
	out = reImg.ReplaceAllString(out, `<figure><img src="$1" alt="$2" loading="lazy"><figcaption>$2</figcaption></figure>`)
	return out, toc
}

func docsRoot(mode string) string {
	if mode == modePage {
		return "../"
	}
	return ""
}

func plainIntro(body string) string {
	if i := strings.Index(body, "<p>"); i >= 0 {
		rest := body[i:]
		if j := strings.Index(rest, "</p>"); j > 0 {
			t := html.UnescapeString(reTag.ReplaceAllString(rest[:j], ""))
			t = strings.Join(strings.Fields(t), " ")
			if len(t) > 170 {
				t = t[:167]
				if k := strings.LastIndex(t, " "); k > 100 {
					t = t[:k]
				}
				t += "..."
			}
			return t
		}
	}
	return ""
}

// ---- pages ---------------------------------------------------------------

type pageData struct {
	Title       string
	Description string
	Canonical   string
	Version     string
	Tagline     string
	Chapters    []*chapter
	Current     *chapter
	Prev, Next  *chapter
	Body        template.HTML
	TOC         []tocItem
	Landing     bool
	StyleV      string
	DocsV       string
	Intro       template.HTML
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title>
<meta name="description" content="{{.Description}}">
<meta name="robots" content="index,follow">
<link rel="canonical" href="{{.Canonical}}">
<link rel="icon" href="/favicon.svg" type="image/svg+xml">
<link rel="stylesheet" href="/style.css?v={{.StyleV}}">
<link rel="stylesheet" href="/docs/docs.css?v={{.DocsV}}">
</head>
<body class="docs">
<header class="dtop">
  <a class="dlogo" href="/" aria-label="pgCellar"><svg viewBox="0 0 64 64" fill="currentColor" aria-hidden="true"><path d="M48 6c9 0 16 8 16 20 0 11-6 19-15 21L38 38 34 18c3-8 8-12 14-12z"/><path d="M14 63c0-12 9-19 22-19h17c7 0 11 7 11 19z"/><circle cx="35" cy="33" r="16" class="el-s" stroke-width="2.5"/><path d="M27 42C14 49 6 45 7 31" fill="none" stroke="currentColor" stroke-width="8" stroke-linecap="round"/><path d="M1 3h12c0 8-3 12-6 12S1 11 1 3z"/><path d="M7 14v12" stroke="currentColor" stroke-width="2.6" stroke-linecap="round"/><path d="M3 6h8" class="el-s" stroke-width="1.6" stroke-linecap="round"/><circle cx="32" cy="29" r="2.4" class="el-f"/><path d="M50 18c5 3 7 9 5 15" fill="none" class="el-s" stroke-width="2.2" stroke-linecap="round"/></svg><b>pg<span>Cellar</span></b></a>
  <a class="dname" href="/docs/">Documentation</a>
  <span class="dver">version {{.Version}}</span>
  <nav class="dlinks"><a href="/docs/pgcellar-manual.pdf">Manual (PDF)</a><a href="https://demo.pgcellar.dbfactory.io">Demo</a><a href="/en/">Product</a></nav>
</header>
<div class="dlayout">
<nav class="dside" aria-label="Chapters">
  <ol>
  {{- $cur := .Current}}
  {{range .Chapters}}<li><a href="/docs/{{.Slug}}/"{{if and $cur (eq $cur.Slug .Slug)}} class="on" aria-current="page"{{end}}><span class="n">{{.N}}</span>{{.Title}}</a></li>
  {{end}}
  </ol>
</nav>
<main class="dmain">
{{if .Landing}}
<h1>{{.Title}}</h1>
{{.Intro}}
<ol class="dchapters">
{{range .Chapters}}<li><a href="/docs/{{.Slug}}/"><span class="n">{{.N}}</span><b>{{.Title}}</b></a><span class="ds">{{.Desc}}</span></li>
{{end}}
</ol>
{{else}}
<p class="kicker">Chapter {{.Current.N}} of {{len .Chapters}}</p>
{{if .TOC}}<nav class="dtoc" aria-label="On this page"><b>On this page</b><ul>{{range .TOC}}<li class="l{{.Level}}"><a href="#{{.ID}}">{{.Text}}</a></li>{{end}}</ul></nav>{{end}}
<article>
{{.Body}}
</article>
<nav class="dpn" aria-label="Previous and next chapter">
  {{if .Prev}}<a class="prev" href="/docs/{{.Prev.Slug}}/"><small>Previous</small>{{.Prev.Title}}</a>{{else}}<span></span>{{end}}
  {{if .Next}}<a class="next" href="/docs/{{.Next.Slug}}/"><small>Next</small>{{.Next.Title}}</a>{{else}}<span></span>{{end}}
</nav>
{{end}}
</main>
</div>
<footer class="dfoot"><div>{{.Tagline}}. <a href="https://dbfactory.io">dbfactory.io</a> &middot; <a href="https://stranix.net">stranix.net</a> &middot; <a href="/docs/pgcellar-manual.pdf">Manual (PDF)</a></div></footer>
</body>
</html>
`))

func writePage(m *manual, i int, outDir string) error {
	c := m.Chapters[i]
	d := pageData{
		Title:       c.Title + " - pgCellar manual",
		Description: plainIntro(c.Body),
		Canonical:   siteURL + "/docs/" + c.Slug + "/",
		Version:     m.Version,
		Tagline:     tagline,
		Chapters:    m.Chapters,
		Current:     c,
		Body:        template.HTML(c.Body),
		TOC:         c.TOC,
		StyleV:      m.StyleV,
		DocsV:       m.DocsV,
	}
	if i > 0 {
		d.Prev = m.Chapters[i-1]
	}
	if i < len(m.Chapters)-1 {
		d.Next = m.Chapters[i+1]
	}
	return execTo(pageTmpl, d, filepath.Join(outDir, c.Slug, "index.html"))
}

func writeLanding(m *manual, outDir string) error {
	intro, _ := render(m.IntroMD, modeLand, nil, m)
	d := pageData{
		Title:       "pgCellar documentation",
		Description: "The pgCellar manual: set up PostgreSQL backups with point-in-time recovery and automatic restore drills, on top of Barman.",
		Canonical:   siteURL + "/docs/",
		Version:     m.Version,
		Tagline:     tagline,
		Chapters:    m.Chapters,
		Landing:     true,
		Intro:       template.HTML(intro),
		StyleV:      m.StyleV,
		DocsV:       m.DocsV,
	}
	return execTo(pageTmpl, d, filepath.Join(outDir, "index.html"))
}

var singleTmpl = template.Must(template.New("single").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>pgCellar manual</title>
<meta name="robots" content="noindex">
<link rel="stylesheet" href="docs.css">
</head>
<body class="manual" id="top">
<section class="cover">
  <svg class="elephant" viewBox="0 0 64 64" fill="currentColor" aria-hidden="true"><path d="M48 6c9 0 16 8 16 20 0 11-6 19-15 21L38 38 34 18c3-8 8-12 14-12z"/><path d="M14 63c0-12 9-19 22-19h17c7 0 11 7 11 19z"/><circle cx="35" cy="33" r="16" class="el-s" stroke-width="2.5"/><path d="M27 42C14 49 6 45 7 31" fill="none" stroke="currentColor" stroke-width="8" stroke-linecap="round"/><path d="M1 3h12c0 8-3 12-6 12S1 11 1 3z"/><path d="M7 14v12" stroke="currentColor" stroke-width="2.6" stroke-linecap="round"/><path d="M3 6h8" class="el-s" stroke-width="1.6" stroke-linecap="round"/><circle cx="32" cy="29" r="2.4" class="el-f"/><path d="M50 18c5 3 7 9 5 15" fill="none" class="el-s" stroke-width="2.2" stroke-linecap="round"/></svg>
  <div class="brand">pg<span>Cellar</span></div>
  <h1 class="covertitle">Manual</h1>
  <p class="tag">{{.Tagline}}</p>
  <p class="coverver">Version {{.Version}}</p>
  <p class="coverfoot">dbfactory.io<br>pgcellar.dbfactory.io/docs</p>
</section>
<section class="contents">
  <p class="tochead">Contents</p>
  <ol class="toclist">
  {{range .Chapters}}<li class="c1"><a href="#{{.Slug}}"><span class="n">{{.N}}</span>{{.Title}}</a>
    <ol>{{range .TOC}}{{if eq .Level 2}}<li class="c2"><a href="#{{.ID}}">{{.Text}}</a></li>{{end}}{{end}}</ol></li>
  {{end}}
  </ol>
</section>
{{range .Chapters}}
<section class="chapter">
<p class="chapnum">Chapter {{.N}}</p>
{{.BodyHTML}}
</section>
{{end}}
</body>
</html>
`))

type singleChapter struct {
	*chapter
	BodyHTML template.HTML
}

func writeSingle(m *manual, path string) error {
	chs := make([]singleChapter, len(m.Chapters))
	for i, c := range m.Chapters {
		chs[i] = singleChapter{c, template.HTML(c.Body)}
	}
	data := struct {
		Tagline, Version string
		Chapters         []singleChapter
	}{tagline, m.Version, chs}
	// The contents list needs .Title/.Slug/.N/.TOC of the embedded chapter.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := singleTmpl.Execute(&buf, data); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func execTo(t *template.Template, data any, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// ---- sitemap ---------------------------------------------------------------

func updateSitemap(path string, m *manual) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var keep []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, siteURL+"/docs/") {
			continue
		}
		keep = append(keep, line)
	}
	text := strings.Join(keep, "\n")
	i := strings.LastIndex(text, "</urlset>")
	if i < 0 {
		return fmt.Errorf("%s has no </urlset>", path)
	}
	var add strings.Builder
	add.WriteString("  <url><loc>" + siteURL + "/docs/</loc></url>\n")
	for _, c := range m.Chapters {
		add.WriteString("  <url><loc>" + siteURL + "/docs/" + c.Slug + "/</loc></url>\n")
	}
	text = text[:i] + add.String() + text[i:]
	return os.WriteFile(path, []byte(text), 0o644)
}

// ---- images ----------------------------------------------------------------

// ensureImages writes a grey placeholder for each screenshot that is missing.
func ensureImages(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range screenshots {
		p := filepath.Join(dir, name+".png")
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if err := writePlaceholder(p); err != nil {
			return err
		}
		fmt.Println("placeholder written:", p)
	}
	return nil
}

func writePlaceholder(path string) error {
	const w, h = 1280, 800
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	fill, edge := color.RGBA{0xE4, 0xE4, 0xE4, 0xFF}, color.RGBA{0xBB, 0xBB, 0xBB, 0xFF}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x < 2 || y < 2 || x >= w-2 || y >= h-2 {
				img.Set(x, y, edge)
			} else {
				img.Set(x, y, fill)
			}
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	return enc.Encode(f, img)
}

func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// fingerprint returns the first 12 hex digits of the SHA-256 of a file, the
// same scheme the application uses for its own assets.
func fingerprint(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12], nil
}

var styleLink = regexp.MustCompile(`href="/style\.css(\?v=[0-9a-f]*)?"`)

// stampHomePages writes the stylesheet fingerprint into the two home pages.
// They are written by hand, so the fingerprint is set here: this keeps
// "make docs" the only command that builds the site, and a restyle is never
// shown with a cached sheet. Running it twice gives the same files.
func stampHomePages(root, v string) error {
	for _, rel := range []string{"index.html", filepath.Join("en", "index.html")} {
		path := filepath.Join(root, "site", rel)
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if n := len(styleLink.FindAll(b, -1)); n != 1 {
			return fmt.Errorf("%s: expected exactly one href=\"/style.css\" attribute, found %d", path, n)
		}
		out := styleLink.ReplaceAll(b, []byte(`href="/style.css?v=`+v+`"`))
		if bytes.Equal(out, b) {
			continue
		}
		if err := os.WriteFile(path, out, 0o644); err != nil {
			return err
		}
	}
	return nil
}
