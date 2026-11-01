package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestTraverseAndFindEpub(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("top.epub")
	mk("a/one.epub")
	mk("a/b/two.epub")
	mk("a/b/c/three.epub")
	mk("notes.txt")

	for _, c := range []struct {
		maxDepth int
		want     int
	}{
		{0, 1},
		{1, 2},
		{2, 3},
		{3, 4},
		{-1, 4},
	} {
		got, err := traverseAndFindEpub(root, c.maxDepth)
		if err != nil {
			t.Fatalf("maxDepth %d: %v", c.maxDepth, err)
		}
		if len(got) != c.want {
			t.Errorf("maxDepth %d found %d epubs (%v), want %d", c.maxDepth, len(got), got, c.want)
		}
	}

	// An absent folder has no epubs; that is not an error, the output folder
	// does not exist before the first book is converted.
	got, err := traverseAndFindEpub(filepath.Join(root, "does-not-exist"), 3)
	if err != nil {
		t.Errorf("missing folder: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("missing folder returned %v", got)
	}
	if got, err := traverseAndFindEpub("", 3); err != nil || len(got) != 0 {
		t.Errorf("empty path: %v, %v", got, err)
	}
}

func TestSplitPatterns(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a,b", "a|b"},
		{" ^toc$ , ^index$ ", "^toc$|^index$"},
		{`^notes(\,|:)$,^index$`, "^notes(,|:)$|^index$"},
		{"", ""},
		{",,", ""},
	}
	for _, c := range cases {
		got := splitPatterns(c.in)
		if join := joinForTest(got); join != c.want {
			t.Errorf("splitPatterns(%q) = %v, want %q", c.in, got, c.want)
		}
	}
}

func joinForTest(list []string) string {
	out := ""
	for i, s := range list {
		if i > 0 {
			out += "|"
		}
		out += s
	}
	return out
}

func TestDefaultSkipPatternsCompile(t *testing.T) {
	if _, err := compilePatterns(defaultSkipPatterns); err != nil {
		t.Fatalf("default skip patterns: %v", err)
	}
}

func TestBooksAreRetriedOnlyUpToMaxAttempts(t *testing.T) {
	requireFFMpeg(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "in")
	out := filepath.Join(dir, "out")
	fixture := writeFixtureEpub(t, filepath.Join(src, "Broken Book.epub"), true)

	// An audiblez that fails without producing anything.
	if _, err := exec.LookPath("python3"); err == nil {
		bin := filepath.Join(dir, "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		script := "#!/bin/sh\necho 'simulated failure' >&2\nexit 1\n"
		if err := os.WriteFile(filepath.Join(bin, "audiblez"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	} else if _, err := exec.LookPath("audiblez"); err != nil {
		t.Skip("neither audiblez nor python3 available")
	}
	_ = fixture

	cfg := defaultConfig()
	cfg.FolderIn = src
	cfg.FolderOut = out
	cfg.MaxAttempts = 2

	books := map[string]*Book{}
	for round := 1; round <= 3; round++ {
		scanFolder(books, cfg, cfg.FolderIn, true)
		scanFolder(books, cfg, cfg.FolderOut, false)
		processBooks(books, cfg)
		book := books["Broken Book.epub"]
		if book == nil {
			t.Fatalf("round %d: book not registered", round)
		}
		if book.Status == Done {
			t.Fatalf("round %d: a failing book must not be Done", round)
		}
		switch round {
		case 1, 2:
			if book.Attempts != round {
				t.Errorf("round %d: attempts = %d", round, book.Attempts)
			}
		case 3:
			if book.Attempts != 2 {
				t.Errorf("gave up after %d attempts, wanted 2", book.Attempts)
			}
			if book.LastError == "" {
				t.Error("the failure reason was not recorded")
			}
		}
	}
	// Nothing was announced as finished, and the input epub was not marked done.
	if _, err := os.Stat(filepath.Join(out, "Broken Book", "Broken Book.epub")); err == nil {
		t.Error("a failed conversion must not copy the epub to the output folder")
	}
}
