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

func TestDerivedEpubOnShippedBook(t *testing.T) {
	if _, err := os.Stat(realBook); err != nil {
		t.Skip("sample book not present")
	}
	b, err := openEpub(realBook)
	if err != nil {
		t.Fatalf("openEpub: %v", err)
	}
	chapters := b.selectedChapters(defaultFilter(t))
	dir := t.TempDir()
	dest := filepath.Join(dir, derivedEpubName)
	names, err := b.writeDerivedEpub(dest, chapters)
	if err != nil {
		t.Fatalf("writeDerivedEpub: %v", err)
	}
	if len(names) != len(chapters) {
		t.Fatalf("names %v", names)
	}
	if names[0] != "OEBPS/chapter_01.xhtml" {
		t.Errorf("unexpected canonical name %q", names[0])
	}
	// The derived book must be readable and expose exactly the chapters.
	derived, err := openEpub(dest)
	if err != nil {
		t.Fatalf("reopening derived epub: %v", err)
	}
	again := derived.selectedChapters(defaultFilter(t))
	if len(again) != len(chapters) {
		t.Fatalf("derived epub has %d chapters, want %d", len(again), len(chapters))
	}
	for i := range chapters {
		if again[i].Title != chapters[i].Title {
			t.Errorf("derived chapter %d: got %q want %q", i+1, again[i].Title, chapters[i].Title)
		}
	}
	if derived.meta.Title != b.meta.Title {
		t.Errorf("derived metadata title %q", derived.meta.Title)
	}
	if derived.coverID == "" {
		t.Errorf("derived epub lost its cover")
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("derived epub: %d bytes (original %d), first chapter %s",
		info.Size(), mustSize(t, realBook), names[0])
}

func mustSize(t *testing.T, p string) int64 {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		return -1
	}
	return st.Size()
}

func TestDerivedEpubKeepsRelativeHrefs(t *testing.T) {
	// The fixture keeps its package document inside OEBPS, like most books: the
	// manifest and navMap hrefs of the derived book have to be relative to it.
	dir := t.TempDir()
	src := writeFixtureEpub(t, filepath.Join(dir, "Fix.epub"), true)
	b, err := openEpub(src)
	if err != nil {
		t.Fatalf("openEpub: %v", err)
	}
	chapters := b.selectedChapters(defaultFilter(t))
	if len(chapters) != 2 {
		t.Fatalf("selected %d chapters, want 2", len(chapters))
	}
	dest := filepath.Join(dir, "derived.epub")
	names, err := b.writeDerivedEpub(dest, chapters)
	if err != nil {
		t.Fatalf("writeDerivedEpub: %v", err)
	}
	if names[0] != "OEBPS/chapter_01.xhtml" {
		t.Errorf("unexpected chapter name %q", names[0])
	}
	derived, err := openEpub(dest)
	if err != nil {
		t.Fatalf("reopen derived: %v", err)
	}
	if derived.ncxPath != "OEBPS/toc.ncx" {
		t.Errorf("ncx path %q", derived.ncxPath)
	}
	again := derived.selectedChapters(defaultFilter(t))
	if len(again) != 2 {
		t.Fatalf("derived book has %d chapters, want 2", len(again))
	}
	for i := range again {
		if again[i].Title != chapters[i].Title {
			t.Errorf("chapter %d: got %q want %q", i, again[i].Title, chapters[i].Title)
		}
		// An href that does not resolve reads as an empty document: the text is
		// what proves manifest, spine and navMap all point at the real file.
		if again[i].charCount() < 1000 {
			t.Errorf("chapter %d (%s) has %d characters", i+1, again[i].Title, again[i].charCount())
		}
	}
	if derived.coverID == "" {
		t.Errorf("derived epub lost its cover")
	}
}

// TestChaptersOutsideThePackageDirectory covers books whose content lives in a
// directory next to the one holding content.opf (a "Text/" folder beside
// "OEBPS/"): the hrefs of those documents start with "..".
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
	names, want := []string{}, []string{"Chapter One", "Chapter Two"}
	for _, ch := range b.spine {
		if keep, _ := b.keepChapter(ch, f); keep {
			names = append(names, ch.Title)
		}
	}
	if len(names) != len(want) {
		t.Fatalf("chapters = %v, want %v", names, want)
	}

	out := filepath.Join(dir, "derived.epub")
	if _, err := b.writeDerivedEpub(out, b.selectedChapters(f)); err != nil {
		t.Fatal(err)
	}
	// ebooklib reads the spine through the manifest hrefs: if they do not point
	// at the entries that were written, the conversion produces silence.
	got, err := openEpub(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.spine) != len(want) {
		t.Fatalf("derived spine = %d documents, want %d", len(got.spine), len(want))
	}
	for i, ch := range got.spine {
		if ch.ZipPath == "" || ch.charCount() < 200 {
			t.Errorf("derived chapter %d = path %q, %d characters", i+1, ch.ZipPath, ch.charCount())
		}
		if !strings.Contains(ch.Title, want[i]) {
			t.Errorf("derived chapter %d title = %q, want %q", i+1, ch.Title, want[i])
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
	relCases := []struct{ dir, zip, want string }{
		{"", "chapter_01.xhtml", "chapter_01.xhtml"},
		{".", "OEBPS/chapter_01.xhtml", "OEBPS/chapter_01.xhtml"},
		{"OEBPS", "OEBPS/chapter_01.xhtml", "chapter_01.xhtml"},
		{"OEBPS", "Text/chapter_01.xhtml", "../Text/chapter_01.xhtml"},
		{"A/B", "A/C/chapter_01.xhtml", "../C/chapter_01.xhtml"},
		{"A/B/C", "A/B/C/D/chapter_01.xhtml", "D/chapter_01.xhtml"},
	}
	for _, c := range relCases {
		if got := relHref(c.dir, c.zip); got != c.want {
			t.Errorf("relHref(%q, %q) = %q, want %q", c.dir, c.zip, got, c.want)
		}
	}

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
