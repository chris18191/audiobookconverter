package main

import (
	"os"
	"path/filepath"
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
	prev := false
	for _, c := range b.spine {
		keep, reason := b.keepChapter(c, f, prev)
		prev = keep
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
		name     string
		ch       epubChapter
		prevKept bool
		filter   chapterFilter
		keep     bool
		reason   string
	}{
		{
			name:   "title page",
			ch:     epubChapter{Title: "cP", Text: text(50), FromHead: true},
			filter: f, reason: "too little text",
		},
		{
			name:     "split back matter tail is a continuation",
			ch:       epubChapter{Title: "c2WT", Text: text(30000)},
			prevKept: true, filter: f, keep: true, reason: "continuation of previous chapter",
		},
		{
			name:   "unnamed document at the start",
			ch:     epubChapter{Title: "c9", Text: text(30000)},
			filter: f, reason: "untitled",
		},
		{
			name:     "unnamed document without enough text",
			ch:       epubChapter{Title: "c9", Text: text(300)},
			prevKept: true, filter: f, reason: "untitled",
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
		keep, reason := (&epubBook{}).keepChapter(&ch, c.filter, c.prevKept)
		if keep != c.keep || reason != c.reason {
			t.Errorf("%s: got (%v, %q), want (%v, %q)", c.name, keep, reason, c.keep, c.reason)
		}
	}
}
