package main

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const realBook = "What Is Intelligence - Blaise Aguera y Arcas.epub"

func defaultFilter(t *testing.T) chapterFilter {
	t.Helper()
	skip, err := compilePatterns(defaultSkipPatterns)
	if err != nil {
		t.Fatalf("compile defaults: %v", err)
	}
	return chapterFilter{MinTextChars: defaultMinText, SkipPatterns: skip}
}

func TestSelectionOnShippedBook(t *testing.T) {
	if _, err := os.Stat(realBook); err != nil {
		t.Skip("sample book not present")
	}
	b, err := openEpub(realBook)
	if err != nil {
		t.Fatalf("openEpub: %v", err)
	}
	if b.meta.Title == "" || b.meta.Creator == "" {
		t.Errorf("metadata not read: %+v", b.meta)
	}
	got := b.selectedChapters(defaultFilter(t))
	want := []string{
		"Foreword by Benjamin Bratton",
		"Preface",
		"Introduction",
		"1. Origins",
		"2. Survival",
		"Interlude: The Prehistory of Computing",
		"3. Cybernetics",
		"4. Learning",
		"5. Other Minds",
		"6. Many Worlds",
		"7. Ourselves",
		"8. Transformers",
		"9. Generality",
		"Interlude: No Perfect Heroes or Villains",
		"10. Evolutionary Transition",
	}
	var names []string
	for _, c := range got {
		names = append(names, c.Title)
		t.Logf("%d. %s (%d chars, toc=%v heading=%v)", len(names), c.Title, c.charCount(), c.FromTOC, c.FromHead)
	}
	if len(names) != len(want) {
		t.Fatalf("got %d chapters, want %d: %v", len(names), len(want), names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("chapter %d: got %q want %q", i+1, names[i], want[i])
		}
	}
	// The excluded documents, for the record: cover/title page, half title,
	// copyright, "A note on the e-book", the generated Contents page,
	// acknowledgments, about the author, the series page, glossary, and both
	// bibliography files (one of which calibre split in two).
	f := defaultFilter(t)
	for _, c := range b.spine {
		keep, reason := b.keepChapter(c, f)
		t.Logf("%-28s keep=%-5v %-32s %q", c.ZipPath, keep, reason, c.Title)
	}
}

// TestChaptersOutsideThePackageDirectory covers books whose content lives in a
// directory next to the one holding content.opf (a "Text/" folder beside
// "OEBPS/"): the hrefs of those documents start with "..", and resolving them
// wrong would read every chapter as an empty document.
func TestChaptersOutsideThePackageDirectory(t *testing.T) {
	dir := t.TempDir()
	src := writeFixtureEpub(t, filepath.Join(dir, "src", "Book.epub"), true)
	flat := filepath.Join(dir, "sibling.epub")
	relocateContent(t, src, flat)

	b, err := openEpub(flat)
	if err != nil {
		t.Fatal(err)
	}
	f := defaultFilter(t)
	sel := b.selectedChapters(f)
	want := []string{"Chapter One", "Chapter Two"}
	if len(sel) != len(want) {
		t.Fatalf("chapters = %d, want %d", len(sel), len(want))
	}
	for i, w := range want {
		if !strings.Contains(sel[i].Title, w) {
			t.Errorf("chapter %d title = %q, want %q", i+1, sel[i].Title, w)
		}
		// A document whose href does not resolve reads as empty: the text is
		// what proves the manifest hrefs were followed correctly.
		if sel[i].charCount() < 1000 {
			t.Errorf("chapter %d (%s) reads as %d characters - \"..\" href lost",
				i+1, sel[i].Title, sel[i].charCount())
		}
	}
}

// relocateContent rewrites an epub so that everything but the package document
// moves from OEBPS/ to the sibling directory Text/, adjusting the manifest hrefs
// the way such a book does.
func relocateContent(t *testing.T, src, dest string) {
	t.Helper()
	zr, err := zip.OpenReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		name := f.Name
		if name != "OEBPS/content.opf" && strings.HasPrefix(name, "OEBPS/") {
			name = "Text/" + strings.TrimPrefix(name, "OEBPS/")
		}
		if name == "OEBPS/content.opf" {
			text := regexp.MustCompile(`href="([^"#]+)"`).ReplaceAllString(string(body), `href="../Text/$1"`)
			body = []byte(text)
		}
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHrefHelpers(t *testing.T) {
	joinCases := []struct{ base, href, want string }{
		{"OEBPS", "c1.xhtml", "OEBPS/c1.xhtml"},
		{"OEBPS", "../Text/c1.xhtml", "Text/c1.xhtml"},
		{"OEBPS", "./c1.xhtml", "OEBPS/c1.xhtml"},
		{"OEBPS", "c1.xhtml#part", "OEBPS/c1.xhtml"},
		{"", "OEBPS/c1.xhtml", "OEBPS/c1.xhtml"},
		// An href starting at the root of the container (legal in EPUB 2).
		{"OEBPS", "/OEBPS/text/c1.xhtml", "OEBPS/text/c1.xhtml"},
		{"OEBPS", "/c1.xhtml", "c1.xhtml"},
		// Percent encoded file names, including non ascii ones.
		{"OEBPS", "the%20chapter.xhtml", "OEBPS/the chapter.xhtml"},
		{"OEBPS", "%D8%B1.xhtml", "OEBPS/ر.xhtml"},
	}
	for _, c := range joinCases {
		if got := joinHref(c.base, c.href); got != c.want {
			t.Errorf("joinHref(%q, %q) = %q, want %q", c.base, c.href, got, c.want)
		}
	}
	// A manifest href and the toc href for the same document have to produce the
	// same zip path, or the chapter loses its title: the two documents live in
	// different directories.
	if joinHref("OEBPS", "c1.xhtml") != joinHref("OEBPS/Text", "../c1.xhtml") {
		t.Error("the same document resolved to two different zip paths")
	}
}

func TestKeepChapterRules(t *testing.T) {
	skip, err := compilePatterns(defaultSkipPatterns)
	if err != nil {
		t.Fatal(err)
	}
	keepOne, err := compilePatterns([]string{`^references$`})
	if err != nil {
		t.Fatal(err)
	}
	f := chapterFilter{MinTextChars: defaultMinText, SkipPatterns: skip}
	fWithKeep := chapterFilter{MinTextChars: defaultMinText, SkipPatterns: skip, KeepPatterns: keepOne}
	text := func(n int) string { return strings.Repeat("x", n) }

	cases := []struct {
		name   string
		book   *epubBook
		ch     epubChapter
		filter chapterFilter
		keep   bool
		reason string
	}{
		{
			name:   "title page",
			ch:     epubChapter{Title: "cP", Text: text(50), FromHead: true},
			filter: f, reason: "too little text",
		},
		{
			name:   "unnamed document in a book without a toc",
			ch:     epubChapter{Title: "c2WT", Text: text(30000)},
			filter: f, keep: true, reason: "untitled but substantial",
		},
		{
			name:   "unnamed document a toc does not mention",
			book:   &epubBook{hasTOC: true},
			ch:     epubChapter{Title: "c2WT", Text: text(30000)},
			filter: f, reason: "not in the table of contents",
		},
		{
			name:   "unnamed document without enough text",
			ch:     epubChapter{Title: "c9", Text: text(300)},
			filter: f, reason: "untitled",
		},
		{
			name:   "short named document",
			ch:     epubChapter{Title: "Dedication", Text: text(60), FromHead: true},
			filter: f, reason: "too little text",
		},
		{
			name:   "toc page by link ratio",
			ch:     epubChapter{Title: "Contents", Text: text(1000), LinkCount: 25, LinkChars: 950},
			filter: f, reason: "table of contents",
		},
		{
			name:   "many links but real prose",
			ch:     epubChapter{Title: "Notes", Text: text(10000), LinkCount: 25, LinkChars: 900, FromHead: true},
			filter: f, keep: true, reason: "named chapter",
		},
		{
			name:   "bibliography",
			ch:     epubChapter{Title: "Bibliography", Text: text(20000), FromTOC: true},
			filter: f, reason: "title matches skip pattern",
		},
		{
			name:   "keep pattern wins over skip pattern",
			ch:     epubChapter{Title: "References", Text: text(20000), FromTOC: true},
			filter: fWithKeep, keep: true, reason: "keep pattern",
		},
		{
			name:   "epub:type page-list",
			ch:     epubChapter{Title: "Index", Text: text(20000), FromTOC: true, Roles: map[string]bool{"index": true}},
			filter: f, reason: "epub:type=index",
		},
		{
			name:   "content role keeps an unnamed document",
			ch:     epubChapter{Title: "c2VS", Text: text(20000), Roles: map[string]bool{"bodymatter": true}},
			filter: f, keep: true, reason: "content role",
		},
		{
			name:   "prologue is content",
			ch:     epubChapter{Title: "Prologue", Text: text(4000), FromHead: true, Roles: map[string]bool{"prologue": true}},
			filter: f, keep: true, reason: "named chapter",
		},
	}
	for _, c := range cases {
		ch := c.ch
		book := c.book
		if book == nil {
			book = &epubBook{}
		}
		keep, reason := book.keepChapter(&ch, c.filter)
		if keep != c.keep || reason != c.reason {
			t.Errorf("%s: got (%v, %q), want (%v, %q)", c.name, keep, reason, c.keep, c.reason)
		}
	}
}
