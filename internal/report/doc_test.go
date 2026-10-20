package report

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func sample(rows int) *Doc {
	d := New("Test report")
	d.FooterL = "pgCellar test"
	d.Header("pgCellar", "Test", []string{" /\\_/\\ ", "( o.o )"})
	d.Banner(true, "RESTORE VERIFIED")
	d.Heading("Steps")
	var data [][]string
	for i := 0; i < rows; i++ {
		data = append(data, []string{fmt.Sprintf("step %d (déjà vu) \\ ( )", i), "+ok", "1 s", strings.Repeat("a long note ", 6)})
	}
	d.Table([]Column{{"Step", 3, false}, {"Result", 1, false}, {"Time", 1, true}, {"Note", 4, false}}, data)
	d.Paragraph("Done ✔ with ✨ and € and 日本.", 10)
	return d
}

func TestPDFStructureOnePage(t *testing.T) {
	pdf, fp := sample(3).Bytes()
	n, err := Check(pdf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 8 { // catalog, pages, 3 fonts, info, page, content
		t.Errorf("objects = %d, want 8", n)
	}
	if len(fp) != 64 {
		t.Errorf("fingerprint %q", fp)
	}
	if !bytes.Contains(pdf, []byte(fp)) {
		t.Error("the fingerprint is not in the footer")
	}
	if !bytes.Contains(pdf, []byte("Page 1 of 1")) {
		t.Error("no page number")
	}
}

func TestPDFSeveralPages(t *testing.T) {
	pdf, _ := sample(120).Bytes()
	n, err := Check(pdf)
	if err != nil {
		t.Fatal(err)
	}
	pages := (n - 6) / 2
	if pages < 3 {
		t.Fatalf("expected several pages, got %d", pages)
	}
	if !bytes.Contains(pdf, []byte(fmt.Sprintf("Page %d of %d", pages, pages))) {
		t.Error("last page number missing")
	}
	if bytes.Count(pdf, []byte("(Step)")) != pages {
		t.Errorf("the table header should be repeated on each of the %d pages", pages)
	}
}

func TestPDFTextEncodingAndEscapes(t *testing.T) {
	pdf, _ := sample(1).Bytes()
	if !bytes.Contains(pdf, []byte(`step 0 \(d\351j\340 vu\) \\ \( \)`)) {
		t.Errorf("accents and parentheses are not encoded as expected")
	}
	if !bytes.Contains(pdf, []byte("Done OK with * and \\200 and ??.")) {
		t.Errorf("fallbacks wrong")
	}
	// Only ASCII in the file apart from the binary marker comment.
	body := pdf[bytes.IndexByte(pdf, '\n')+1:]
	body = body[bytes.IndexByte(body, '\n')+1:]
	for i, c := range body {
		if c > 0x7e || (c < 0x20 && c != '\n') {
			t.Fatalf("unexpected byte 0x%02x at %d", c, i)
		}
	}
}

func TestFingerprintDependsOnContent(t *testing.T) {
	_, a := sample(2).Bytes()
	_, b := sample(2).Bytes()
	_, c := sample(3).Bytes()
	if a != b {
		t.Error("same content, different fingerprint")
	}
	if a == c {
		t.Error("different content, same fingerprint")
	}
}

func TestCheckRejectsDamage(t *testing.T) {
	pdf, _ := sample(2).Bytes()
	bad := bytes.Replace(pdf, []byte("1 0 obj"), []byte("9 0 obj"), 1)
	if _, err := Check(bad); err == nil {
		t.Error("a moved object must be refused")
	}
	if _, err := Check(pdf[:len(pdf)-20]); err == nil {
		t.Error("a truncated file must be refused")
	}
	if _, err := Check([]byte("hello")); err == nil {
		t.Error("not a PDF")
	}
}

func TestWrapCutsLongWords(t *testing.T) {
	for _, l := range wrap(strings.Repeat("x", 300), fontRegular, 10, 100) {
		if Width(l, fontRegular, 10) > 100.01 {
			t.Fatalf("line too wide: %d chars", len(l))
		}
	}
}
