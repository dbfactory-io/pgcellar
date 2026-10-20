package report

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
)

var (
	startxrefRe = regexp.MustCompile(`startxref\n(\d+)\n%%EOF\n?$`)
	lengthRe    = regexp.MustCompile(`/Length (\d+) >>\nstream\n`)
)

// Check reads a PDF written by this package the way a viewer starts: header,
// startxref, the cross-reference table, then every offset must point at
// "<n> 0 obj", every stream must be exactly /Length bytes long, and the trailer
// must name a root object that exists. It returns the number of objects.
func Check(pdf []byte) (int, error) {
	if !bytes.HasPrefix(pdf, []byte("%PDF-1.4\n")) {
		return 0, fmt.Errorf("missing %%PDF-1.4 header")
	}
	m := startxrefRe.FindSubmatch(pdf)
	if m == nil {
		return 0, fmt.Errorf("missing startxref / %%%%EOF")
	}
	pos, _ := strconv.Atoi(string(m[1]))
	if pos <= 0 || pos >= len(pdf) || !bytes.HasPrefix(pdf[pos:], []byte("xref\n")) {
		return 0, fmt.Errorf("startxref %d does not point at the xref table", pos)
	}
	rest := pdf[pos+len("xref\n"):]
	var first, count int
	nl := bytes.IndexByte(rest, '\n')
	if nl < 0 {
		return 0, fmt.Errorf("bad xref subsection header")
	}
	if _, err := fmt.Sscanf(string(rest[:nl]), "%d %d", &first, &count); err != nil || first != 0 || count < 2 {
		return 0, fmt.Errorf("bad xref subsection header")
	}
	n := nl + 1
	rest = rest[n:]
	if len(rest) < count*20 {
		return 0, fmt.Errorf("xref table is cut short")
	}
	if string(rest[:20]) != "0000000000 65535 f \n" {
		return 0, fmt.Errorf("first xref entry must be the free head")
	}
	for i := 1; i < count; i++ {
		e := rest[i*20 : (i+1)*20]
		if e[10] != ' ' || e[16] != ' ' || e[17] != 'n' || e[18] != ' ' || e[19] != '\n' {
			return 0, fmt.Errorf("xref entry %d is malformed: %q", i, e)
		}
		off, err := strconv.Atoi(string(e[:10]))
		if err != nil || off <= 0 || off >= pos {
			return 0, fmt.Errorf("xref entry %d: bad offset %q", i, e[:10])
		}
		want := fmt.Sprintf("%d 0 obj\n", i)
		if !bytes.HasPrefix(pdf[off:], []byte(want)) {
			return 0, fmt.Errorf("xref entry %d: offset %d does not point at %q", i, off, want)
		}
		end := bytes.Index(pdf[off:], []byte("endobj\n"))
		if end < 0 {
			return 0, fmt.Errorf("object %d has no endobj", i)
		}
		body := pdf[off : off+end]
		if lm := lengthRe.FindSubmatchIndex(body); lm != nil {
			l, _ := strconv.Atoi(string(body[lm[2]:lm[3]]))
			s := body[lm[1]:]
			if !bytes.HasSuffix(s, []byte("endstream\n")) && !bytes.HasSuffix(s, []byte("endstream")) {
				return 0, fmt.Errorf("object %d: no endstream", i)
			}
			if got := bytes.LastIndex(s, []byte("endstream")); got != l {
				return 0, fmt.Errorf("object %d: /Length is %d but the stream is %d bytes", i, l, got)
			}
		}
	}
	tr := pdf[pos+len("xref\n")+n+count*20:]
	var size, root int
	if _, err := fmt.Sscanf(string(tr), "trailer\n<< /Size %d /Root %d 0 R", &size, &root); err != nil {
		return 0, fmt.Errorf("bad trailer: %v", err)
	}
	if size != count || root < 1 || root >= count {
		return 0, fmt.Errorf("trailer says size %d root %d for %d entries", size, root, count)
	}
	return count - 1, nil
}
