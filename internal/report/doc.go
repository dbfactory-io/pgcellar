// Package report writes small PDF documents without any dependency: PDF 1.4,
// the standard Helvetica and Courier fonts, text, lines, rectangles and simple
// tables over several A4 pages. It exists for the restore drill report; it is
// not a general PDF library.
//
// Text is limited to what the standard fonts can show (Windows-1252, which is
// Latin-1 plus a few typographic marks). Other characters are replaced by a
// plain equivalent or by "?".
package report

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"
	"unicode/utf8"
)

// Page geometry, in points (1/72 inch). A4 portrait.
const (
	pageW   = 595.28
	pageH   = 841.89
	margin  = 50.0
	footerH = 40.0
)

// Fonts.
const (
	fontRegular = iota
	fontBold
	fontMono
)

var fontNames = []string{"Helvetica", "Helvetica-Bold", "Courier"}

// Doc is a document under construction. Content goes from top to bottom; a
// new page starts by itself when the content does not fit.
type Doc struct {
	Title    string // the /Title of the PDF
	FooterL  string // left part of the footer, repeated on every page
	pages    []*strings.Builder
	y        float64 // distance from the top of the current page
	sum      hash.Hash
	finished bool
}

// New starts a document.
func New(title string) *Doc {
	d := &Doc{Title: title, sum: sha256.New()}
	d.newPage()
	return d
}

func (d *Doc) newPage() {
	d.pages = append(d.pages, &strings.Builder{})
	d.y = margin
	d.sum.Write([]byte("\f"))
}

func (d *Doc) cur() *strings.Builder { return d.pages[len(d.pages)-1] }

// room reports whether h points still fit on the page.
func (d *Doc) room(h float64) bool { return d.y+h <= pageH-margin-footerH }

func (d *Doc) ensure(h float64) {
	if !d.room(h) {
		d.newPage()
	}
}

// Space leaves a vertical gap.
func (d *Doc) Space(pt float64) { d.y += pt }

// ---- low level drawing (y from the top) ----

func f2(v float64) string { return fmt.Sprintf("%.2f", v) }

func (d *Doc) rgb(c [3]float64, stroke bool) {
	op := "rg"
	if stroke {
		op = "RG"
	}
	fmt.Fprintf(d.cur(), "%.3f %.3f %.3f %s\n", c[0], c[1], c[2], op)
}

// Colors.
var (
	Black = [3]float64{0.1, 0.1, 0.1}
	Gray  = [3]float64{0.45, 0.45, 0.45}
	Light = [3]float64{0.93, 0.94, 0.95}
	Green = [3]float64{0.10, 0.55, 0.30}
	Red   = [3]float64{0.78, 0.15, 0.15}
	Blue  = [3]float64{0.20, 0.40, 0.57}
	White = [3]float64{1, 1, 1}
)

func (d *Doc) textAt(x, ytop float64, font int, size float64, c [3]float64, s string) {
	d.sum.Write([]byte(s + "\n"))
	d.drawText(d.cur(), x, ytop, font, size, c, s)
}

func (d *Doc) drawText(b *strings.Builder, x, ytop float64, font int, size float64, c [3]float64, s string) {
	fmt.Fprintf(b, "%.3f %.3f %.3f rg\nBT /F%d %s Tf %s %s Td (%s) Tj ET\n",
		c[0], c[1], c[2], font+1, f2(size), f2(x), f2(pageH-ytop), escape(s))
}

func (d *Doc) rectFill(x, ytop, w, h float64, c [3]float64) {
	d.rgb(c, false)
	fmt.Fprintf(d.cur(), "%s %s %s %s re f\n", f2(x), f2(pageH-ytop-h), f2(w), f2(h))
}

func (d *Doc) lineH(x1, x2, ytop, width float64, c [3]float64) {
	d.rgb(c, true)
	fmt.Fprintf(d.cur(), "%s w %s %s m %s %s l S\n", f2(width), f2(x1), f2(pageH-ytop), f2(x2), f2(pageH-ytop))
}

// ---- encoding ----

// winAnsi maps the runes outside Latin-1 that Windows-1252 has.
var winAnsi = map[rune]byte{
	'€': 0x80, '‚': 0x82, 'ƒ': 0x83, '„': 0x84, '…': 0x85, '†': 0x86, '‡': 0x87, 'ˆ': 0x88, '‰': 0x89,
	'Š': 0x8A, '‹': 0x8B, 'Œ': 0x8C, 'Ž': 0x8E, '‘': 0x91, '’': 0x92, '“': 0x93, '”': 0x94, '•': 0x95,
	'–': 0x96, '—': 0x97, '˜': 0x98, '™': 0x99, 'š': 0x9A, '›': 0x9B, 'œ': 0x9C, 'ž': 0x9E, 'Ÿ': 0x9F,
}

// fallback gives a plain stand-in for common symbols the fonts do not have.
var fallback = map[rune]string{
	'✔': "OK", '✓': "OK", '✘': "X", '✗': "X", '✨': "*", '→': "->", '←': "<-", '≥': ">=", '≤': "<=",
	' ': " ", '\t': " ",
}

// encode turns UTF-8 into WinAnsi bytes; what has no place becomes "?".
func encode(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r':
			out = append(out, ' ')
		case r >= 0x20 && r < 0x7f, r >= 0xa0 && r <= 0xff:
			out = append(out, byte(r))
		default:
			if b, ok := winAnsi[r]; ok {
				out = append(out, b)
			} else if f, ok := fallback[r]; ok {
				out = append(out, f...)
			} else {
				out = append(out, '?')
			}
		}
	}
	return out
}

// escape encodes s and protects the characters that matter inside a PDF string.
func escape(s string) string {
	var b strings.Builder
	for _, c := range encode(s) {
		switch {
		case c == '\\' || c == '(' || c == ')':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c >= 0x80:
			fmt.Fprintf(&b, "\\%03o", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ---- metrics ----

// Advance widths of ASCII 32..126 in 1/1000 em, from the Adobe AFM files.
var helv = [95]int{
	278, 278, 355, 556, 556, 889, 667, 191, 333, 333, 389, 584, 278, 333, 278, 278,
	556, 556, 556, 556, 556, 556, 556, 556, 556, 556, 278, 278, 584, 584, 584, 556,
	1015, 667, 667, 722, 722, 667, 611, 778, 722, 278, 500, 667, 556, 833, 722, 778,
	667, 778, 722, 667, 611, 722, 667, 944, 667, 667, 611, 278, 278, 278, 469, 556,
	333, 556, 556, 500, 556, 556, 278, 556, 556, 222, 222, 500, 222, 833, 556, 556,
	556, 556, 333, 500, 278, 556, 500, 722, 500, 500, 500, 334, 260, 334, 584,
}

var helvBold = [95]int{
	278, 333, 474, 556, 556, 889, 722, 238, 333, 333, 389, 584, 278, 333, 278, 278,
	556, 556, 556, 556, 556, 556, 556, 556, 556, 556, 333, 333, 584, 584, 584, 611,
	975, 722, 722, 722, 722, 667, 611, 778, 722, 278, 556, 722, 611, 833, 722, 778,
	667, 778, 722, 667, 611, 722, 667, 944, 667, 667, 611, 333, 278, 333, 584, 556,
	333, 556, 611, 556, 611, 556, 333, 611, 611, 278, 278, 556, 278, 889, 611, 611,
	611, 611, 389, 556, 333, 611, 556, 778, 556, 556, 500, 389, 280, 389, 584,
}

// Width returns the width in points of s set in the font at the size.
func Width(s string, font int, size float64) float64 {
	n := 0
	for _, c := range encode(s) {
		switch {
		case font == fontMono:
			n += 600
		case c >= 0x20 && c < 0x7f && font == fontBold:
			n += helvBold[c-0x20]
		case c >= 0x20 && c < 0x7f:
			n += helv[c-0x20]
		case font == fontBold:
			n += 611
		default:
			n += 556
		}
	}
	return float64(n) * size / 1000
}

// wrap breaks s into lines no wider than max points. A word longer than a
// line is cut.
func wrap(s string, font int, size, max float64) []string {
	var lines []string
	for _, para := range strings.Split(s, "\n") {
		line := ""
		flush := func() {
			lines = append(lines, line)
			line = ""
		}
		for _, w := range strings.Fields(para) {
			for Width(w, font, size) > max {
				cut := 1
				for cut < utf8.RuneCountInString(w) && Width(string([]rune(w)[:cut+1]), font, size) <= max {
					cut++
				}
				if line != "" {
					flush()
				}
				r := []rune(w)
				lines = append(lines, string(r[:cut]))
				w = string(r[cut:])
			}
			switch {
			case line == "":
				line = w
			case Width(line+" "+w, font, size) <= max:
				line += " " + w
			default:
				flush()
				line = w
			}
		}
		flush()
	}
	if len(lines) == 0 {
		lines = []string{""}
	}
	return lines
}

// ---- building blocks ----

// Header draws the brand line at the top of the first page: the brand at the
// left, a small text drawing at the right, a rule underneath.
func (d *Doc) Header(brand, subtitle string, art []string) {
	top := d.y
	d.textAt(margin, top+22, fontBold, 22, Blue, brand)
	d.textAt(margin, top+40, fontRegular, 10, Gray, subtitle)
	if len(art) > 0 {
		widest := 0.0
		for _, l := range art {
			if w := Width(l, fontMono, 6); w > widest {
				widest = w
			}
		}
		for i, l := range art {
			d.textAt(pageW-margin-widest, top+8+float64(i)*7, fontMono, 6, Gray, l)
		}
		if h := float64(len(art))*7 + 8; h > 44 {
			top += h - 44
		}
	}
	d.y = top + 50
	d.lineH(margin, pageW-margin, d.y, 1, Blue)
	d.y += 14
}

// Heading draws a section title.
func (d *Doc) Heading(s string) {
	d.ensure(40)
	d.y += 8
	d.textAt(margin, d.y+12, fontBold, 12, Black, s)
	d.y += 20
}

// Paragraph draws wrapped text across the page width.
func (d *Doc) Paragraph(s string, size float64) {
	for _, l := range wrap(s, fontRegular, size, pageW-2*margin) {
		d.ensure(size + 4)
		d.textAt(margin, d.y+size, fontRegular, size, Black, l)
		d.y += size + 4
	}
}

// KeyValues draws a two-column list: grey labels, black values.
func (d *Doc) KeyValues(rows [][2]string) {
	const labelW = 130.0
	for _, r := range rows {
		lines := wrap(r[1], fontRegular, 10, pageW-2*margin-labelW)
		d.ensure(float64(len(lines))*13 + 2)
		d.textAt(margin, d.y+10, fontRegular, 10, Gray, r[0])
		for _, l := range lines {
			d.textAt(margin+labelW, d.y+10, fontRegular, 10, Black, l)
			d.y += 13
		}
		d.y += 2
	}
}

// Banner draws the verdict in a coloured box with a drawn check mark or cross
// (the standard fonts have neither).
func (d *Doc) Banner(ok bool, text string) {
	const h = 46.0
	d.ensure(h + 8)
	col := Red
	if ok {
		col = Green
	}
	d.rectFill(margin, d.y, pageW-2*margin, h, col)
	b := d.cur()
	cx, cy := margin+28, pageH-(d.y+h/2)
	d.rgb(White, true)
	if ok {
		fmt.Fprintf(b, "4 w 1 J 1 j %s %s m %s %s l %s %s l S\n", f2(cx-10), f2(cy), f2(cx-3), f2(cy-8), f2(cx+11), f2(cy+10))
	} else {
		fmt.Fprintf(b, "4 w 1 J %s %s m %s %s l S %s %s m %s %s l S\n", f2(cx-9), f2(cy+9), f2(cx+9), f2(cy-9), f2(cx-9), f2(cy-9), f2(cx+9), f2(cy+9))
	}
	d.textAt(margin+52, d.y+h/2+8, fontBold, 22, White, text)
	d.y += h + 10
}

// Column describes a table column: its title, its share of the table width
// and whether the text is aligned to the right.
type Column struct {
	Title string
	Share float64
	Right bool
}

// Table draws a table whose rows may wrap onto several lines. The header is
// repeated after a page break. A cell starting with "!" is drawn in red, one
// starting with "+" in green (the marker is not printed).
func (d *Doc) Table(cols []Column, rows [][]string) {
	const size, lead, pad = 9.0, 11.0, 4.0
	total := 0.0
	for _, c := range cols {
		total += c.Share
	}
	width := pageW - 2*margin
	xs := make([]float64, len(cols)+1)
	xs[0] = margin
	for i, c := range cols {
		xs[i+1] = xs[i] + width*c.Share/total
	}
	header := func() {
		d.ensure(lead + 2*pad + 2)
		d.rectFill(margin, d.y, width, lead+2*pad-2, Light)
		for i, c := range cols {
			d.cell(xs[i], xs[i+1], d.y+pad+size-1, fontBold, size, Black, c.Title, c.Right)
		}
		d.y += lead + 2*pad - 2
	}
	header()
	for _, row := range rows {
		cells := make([][]string, len(cols))
		maxLines := 1
		for i := range cols {
			t := ""
			if i < len(row) {
				t = strings.TrimLeft(row[i], "!+")
			}
			cells[i] = wrap(t, fontRegular, size, xs[i+1]-xs[i]-2*pad)
			if len(cells[i]) > maxLines {
				maxLines = len(cells[i])
			}
		}
		h := float64(maxLines)*lead + 2*pad
		if !d.room(h) {
			d.newPage()
			header()
		}
		for i := range cols {
			c := Black
			if i < len(row) {
				switch {
				case strings.HasPrefix(row[i], "!"):
					c = Red
				case strings.HasPrefix(row[i], "+"):
					c = Green
				}
			}
			for j, l := range cells[i] {
				d.cell(xs[i], xs[i+1], d.y+pad+size-1+float64(j)*lead, fontRegular, size, c, l, cols[i].Right)
			}
		}
		d.y += h
		d.lineH(margin, pageW-margin, d.y, 0.4, [3]float64{0.8, 0.8, 0.8})
	}
	d.y += 6
}

func (d *Doc) cell(x1, x2, ybase float64, font int, size float64, c [3]float64, s string, right bool) {
	const pad = 4.0
	x := x1 + pad
	if right {
		x = x2 - pad - Width(s, font, size)
	}
	d.textAt(x, ybase, font, size, c, s)
}

// Mono draws lines in Courier, cut at the page width, for logs.
func (d *Doc) Mono(lines []string) {
	const size = 7.5
	max := pageW - 2*margin
	for _, l := range lines {
		for _, w := range wrap(l, fontMono, size, max) {
			d.ensure(size + 3)
			d.textAt(margin, d.y+size, fontMono, size, Gray, w)
			d.y += size + 3
		}
	}
}

// Fingerprint is the SHA-256 of the text of the body, in the order it was
// drawn. It does not cover the footers (which carry it) nor the layout.
func (d *Doc) Fingerprint() string { return hex.EncodeToString(d.sum.Sum(nil)) }

// Bytes closes the document and returns the PDF file and the fingerprint.
func (d *Doc) Bytes() ([]byte, string) {
	fp := d.Fingerprint()
	n := len(d.pages)
	for i, p := range d.pages {
		d.rgb2(p, Gray, true)
		fmt.Fprintf(p, "0.4 w %s %s m %s %s l S\n", f2(margin), f2(footerH+10), f2(pageW-margin), f2(footerH+10))
		d.drawText(p, margin, pageH-footerH+6, fontRegular, 7.5, Gray, d.FooterL)
		pg := fmt.Sprintf("Page %d of %d", i+1, n)
		d.drawText(p, pageW-margin-Width(pg, fontRegular, 7.5), pageH-footerH+6, fontRegular, 7.5, Gray, pg)
		d.drawText(p, margin, pageH-footerH+17, fontMono, 6.5, Gray, "SHA-256 of the report content: "+fp)
	}
	return d.build(), fp
}

func (d *Doc) rgb2(b *strings.Builder, c [3]float64, stroke bool) {
	op := "rg"
	if stroke {
		op = "RG"
	}
	fmt.Fprintf(b, "%.3f %.3f %.3f %s\n", c[0], c[1], c[2], op)
}

// build serialises the objects with their cross-reference table.
func (d *Doc) build() []byte {
	var out bytes.Buffer
	var offsets []int
	obj := func(body string) int {
		offsets = append(offsets, out.Len())
		id := len(offsets)
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", id, body)
		return id
	}
	out.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	// Object numbers: 1 catalog, 2 pages, 3..5 fonts, 6 info, then page + content pairs.
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	kids := make([]string, len(d.pages))
	for i := range d.pages {
		kids[i] = fmt.Sprintf("%d 0 R", 7+2*i)
	}
	obj(fmt.Sprintf("<< /Type /Pages /Count %d /Kids [%s] >>", len(d.pages), strings.Join(kids, " ")))
	for _, name := range fontNames {
		obj("<< /Type /Font /Subtype /Type1 /BaseFont /" + name + " /Encoding /WinAnsiEncoding >>")
	}
	obj(fmt.Sprintf("<< /Title (%s) /Producer (pgCellar) >>", escape(d.Title)))
	for i, p := range d.pages {
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %s %s] /Resources << /Font << /F1 3 0 R /F2 4 0 R /F3 5 0 R >> >> /Contents %d 0 R >>",
			f2(pageW), f2(pageH), 8+2*i))
		content := p.String()
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content))
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R /Info 6 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return out.Bytes()
}
