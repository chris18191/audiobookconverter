package main

import (
	"archive/zip"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A 1x1 transparent png, enough for the cover detection.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// writeFixtureEpub writes a small but complete epub: a title page, two chapters
// and a bibliography, with (EPUB3 nav plus NCX) or without a table of contents.
// It is the input of the end to end test.
func writeFixtureEpub(t *testing.T, dest string, withTOC bool) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)

	add := func(name, content string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	mimetype, err := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mimetype.Write([]byte("application/epub+zip")); err != nil {
		t.Fatal(err)
	}
	add("META-INF/container.xml", `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`)

	extraManifest := ""
	extraSpine := ""
	if withTOC {
		extraManifest = `
    <item id="ncx" href="toc.ncx" media-type="application/x-dtbncx+xml"/>
    <item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>`
		extraSpine = `
      <itemref idref="nav"/>`
	}
	add("OEBPS/content.opf", `<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="uid" xml:lang="en">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:identifier id="uid">urn:uuid:11111111-2222-3333-4444-555555555555</dc:identifier>
    <dc:title>The Fixture Book</dc:title>
    <dc:creator>Jane Doe</dc:creator>
    <dc:language>en</dc:language>
    <meta name="cover" content="cover-image"/>
  </metadata>
  <manifest>
    <item id="cover-image" href="cover.png" media-type="image/png" properties="cover-image"/>
    <item id="css" href="style.css" media-type="text/css"/>`+extraManifest+`
    <item id="titlepage" href="titlepage.xhtml" media-type="application/xhtml+xml"/>
    <item id="ch1" href="chapter1.xhtml" media-type="application/xhtml+xml"/>
    <item id="ch2" href="chapter2.xhtml" media-type="application/xhtml+xml"/>
    <item id="refs" href="references.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine>`+extraSpine+`
      <itemref idref="titlepage"/>
      <itemref idref="ch1"/>
      <itemref idref="ch2"/>
      <itemref idref="refs"/>
  </spine>
</package>`)

	doc := func(title, body string) string {
		return `<?xml version="1.0" encoding="utf-8"?>
<html xmlns="http://www.w3.org/1999/xhtml"><head><title>` + title +
			`</title><link rel="stylesheet" type="text/css" href="style.css"/></head><body>` +
			body + `</body></html>`
	}
	add("OEBPS/style.css", "body { margin: 0; }\n")
	add("OEBPS/titlepage.xhtml", doc("titlepage", "<p><img src=\"cover.png\" alt=\"cover\"/></p>"))
	add("OEBPS/chapter1.xhtml", doc("chapter1",
		"<h1>Chapter One</h1><p>"+strings.Repeat("First the world, then the word. ", 80)+"</p>"))
	add("OEBPS/chapter2.xhtml", doc("chapter2",
		"<h1>Chapter Two</h1><p>"+strings.Repeat("Learning is a prediction. ", 80)+"</p>"))
	add("OEBPS/references.xhtml", doc("references",
		"<h1>Bibliography</h1><p>"+strings.Repeat("Doe, J. (2024). Fixtures. ", 20)+"</p>"))
	if withTOC {
		add("OEBPS/nav.xhtml", `<?xml version="1.0" encoding="utf-8"?>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops"><head><title>Contents</title></head><body>
<nav epub:type="toc"><h1>Contents</h1><ol>
  <li><a href="chapter1.xhtml">Chapter One</a><ol><li><a href="chapter1.xhtml#s1">Inside Chapter One</a></li></ol></li>
  <li><a href="chapter2.xhtml">Chapter Two</a></li>
  <li><a href="references.xhtml">Bibliography</a></li>
</ol></nav>
</body></html>`)
		add("OEBPS/toc.ncx", `<?xml version="1.0" encoding="utf-8"?>
<ncx xmlns="http://www.daisy.org/z3986/2005/ncx/" version="2005-1">
  <head><meta name="dtb:uid" content="urn:uuid:11111111-2222-3333-4444-555555555555"/></head>
  <docTitle><text>The Fixture Book</text></docTitle>
  <navMap>
    <navPoint id="n1" playOrder="1"><navLabel><text>Chapter One</text></navLabel><content src="chapter1.xhtml"/></navPoint>
    <navPoint id="n2" playOrder="2"><navLabel><text>Chapter Two</text></navLabel><content src="chapter2.xhtml"/></navPoint>
  </navMap>
</ncx>`)
	}
	png, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}
	w, err := zw.Create("OEBPS/cover.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(png); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return dest
}
