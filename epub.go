package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	nethtml "golang.org/x/net/html"
)

// audiblez 0.4.9 only treats a document as a chapter when its *file name* looks
// like one (audiblez/core.py: is_chapter -> "chapter", /part_\d+/, /split_\d+/,
// /ch_\d+/, /chap_\d+/). Calibre exports use obfuscated names (c9H.xhtml,
// cTZ.xhtml, ...), so for such a book nothing matches except the split-off
// bibliography (c2WT_split_000.xhtml): the whole book is skipped and only the
// references are narrated.
//
// So we select the chapters ourselves (spine + table of contents + text
// density) and hand audiblez a derived epub containing exactly those chapters,
// stored under canonical names ("chapter_01.xhtml" ...) that audiblez always
// recognises, listed in manifest order in the same reading order (audiblez
// iterates book.get_items(), i.e. OPF manifest order, not spine order).

// derivedEpubName is the epub audiblez is run on; audiblez derives its wav file
// names from it: chapters_chapter_<n>_<voice>_<sanitised path>.wav
const derivedEpubName = "chapters.epub"

// audiblezMinChapterChars is the text length audiblez itself requires before it
// writes audio for a chapter (core.py: is_chapter / "Skipping empty chapter").
const audiblezMinChapterChars = 100

// htmlTextTags mirrors the tags audiblez extracts speech from, so that our
// "is there enough text here" test agrees with what audiblez will synthesise.
var htmlTextTags = map[string]bool{
	"p": true, "h1": true, "h2": true, "h3": true, "h4": true, "li": true,
}

// nonContentRoles: EPUB3 structure vocabulary for material that must not be
// narrated. contentRoles: positive signals for otherwise untitled documents.
var (
	nonContentRoles = map[string]bool{
		"toc": true, "toc-body": true, "bibliography": true, "index": true,
		"colophon": true, "imprimatur": true, "copyrights": true,
		"titlepage": true, "halftitlepage": true, "dedication": true,
		"epigraphs": true, "endorsements": true, "contributors": true,
		"acknowledgments": true, "pfb": true, "errata": true,
	}
	contentRoles = map[string]bool{
		"chapter": true, "prologue": true, "epilogue": true, "bodymatter": true,
		"preface": true, "foreword": true, "introduction": true,
	}
)

type manifestItem struct {
	ID         string
	Href       string
	MediaType  string
	Properties string
}

type bookMetadata struct {
	Title      string
	Creator    string
	Identifier string
	Language   string
}

// epubChapter is a spine document plus everything needed to decide whether it is
// spoken content and how its audio file should be named.
type epubChapter struct {
	ZipPath    string
	ManifestID string
	Title      string
	FromTOC    bool // title taken from the book's table of contents
	FromHead   bool // title taken from the document's own first heading
	Text       string
	Roles      map[string]bool
	LinkCount  int
	LinkChars  int
}

// charCount counts characters, the way audiblez measures the text it has to
// speak (Python len()), not bytes.
func (c *epubChapter) charCount() int { return utf8.RuneCountInString(c.Text) }

// named reports whether the chapter has a human readable title to name the
// audio file after.
func (c *epubChapter) named() bool { return c.FromTOC || c.FromHead }

// looksLikeTOC reports whether the document is essentially a list of links,
// which is how a generated "Contents" page looks. Calibre emits one even for
// EPUB2 books, and then the NCX points *into* it (the foreword navPoint targets
// c16.xhtml#a4G4), so labelling alone cannot detect it.
func (c *epubChapter) looksLikeTOC() bool {
	if c.LinkCount < 12 || c.charCount() == 0 {
		return false
	}
	return float64(c.LinkChars)/float64(c.charCount()) >= 0.7
}

type epubBook struct {
	path      string
	meta      bookMetadata
	opfPath   string
	opfDir    string
	packageEl []byte // verbatim "<package ...>" start tag of the original OPF
	metadata  []byte // verbatim "<metadata>...</metadata>" of the original OPF
	items     []manifestItem
	spineIDs  []string
	spine     []*epubChapter // documents in reading order
	ncxPath   string         // path of the book's NCX inside the zip, if any
	hasTOC    bool           // the book has a table of contents with entries
	coverID   string
}

// openEpub reads container, package document and table of contents, and returns
// the spine documents in reading order.
func openEpub(p string) (*epubBook, error) {
	zr, err := zip.OpenReader(p)
	if err != nil {
		return nil, fmt.Errorf("could not open epub %s: %w", p, err)
	}
	defer zr.Close()

	b := &epubBook{path: p}
	opfName, err := rootfilePath(&zr.Reader)
	if err != nil {
		return nil, err
	}
	b.opfPath = opfName
	b.opfDir = path.Dir(opfName)
	if b.opfDir == "." {
		b.opfDir = ""
	}
	opfBytes, err := readZipEntry(&zr.Reader, opfName)
	if err != nil {
		return nil, err
	}
	if err := b.parsePackage(opfBytes); err != nil {
		return nil, err
	}

	labels, err := b.tocLabels(&zr.Reader)
	if err != nil {
		return nil, err
	}
	b.hasTOC = len(labels) > 0

	byID := make(map[string]manifestItem, len(b.items))
	for _, it := range b.items {
		byID[it.ID] = it
	}
	navDoc := b.navPath()
	seen := map[string]bool{}
	for _, idref := range b.spineIDs {
		it, ok := byID[idref]
		if !ok || !isDocument(it) {
			continue
		}
		zipPath := joinHref(b.opfDir, it.Href)
		if zipPath == navDoc {
			continue // the EPUB3 nav document is a table of contents
		}
		if seen[zipPath] {
			// A spine that lists the same document twice would have audiblez
			// speak the section twice.
			log.Printf("'%s' is referenced by the spine more than once: narrating it once", zipPath)
			continue
		}
		seen[zipPath] = true
		ch := &epubChapter{ZipPath: zipPath, ManifestID: it.ID, Roles: map[string]bool{}}
		if raw, err := readZipEntry(&zr.Reader, zipPath); err == nil {
			a := analyseHTML(raw)
			ch.Text = a.Text
			ch.Roles = a.Roles
			ch.LinkCount = a.LinkCount
			ch.LinkChars = a.LinkChars
			if a.Heading != "" {
				ch.Title = a.Heading
				ch.FromHead = true
			} else if a.DocTitle != "" && !strings.EqualFold(a.DocTitle, fileLabel(zipPath)) {
				// Calibre names the <title> of every document after the file,
				// which is no title at all.
				ch.Title = a.DocTitle
				ch.FromHead = true
			}
		} else {
			// Without the text there is nothing to select on, so this document
			// will most likely be dropped: say so instead of hiding it.
			log.Printf("the document %s cannot be read from the epub: %v", zipPath, err)
		}
		if entry, ok := labels[zipPath]; ok && entry.Title != "" {
			ch.Title = entry.Title
			ch.FromTOC = true
			ch.FromHead = false
		}
		if ch.Title == "" {
			ch.Title = fileLabel(zipPath)
		}
		b.spine = append(b.spine, ch)
	}
	b.coverID = b.detectCover()
	return b, nil
}

// parsePackage fills metadata, manifest and spine, keeping the original
// <package> start tag and <metadata> element verbatim so the derived epub keeps
// title, author and cover information (including namespace prefixes).
func (b *epubBook) parsePackage(opf []byte) error {
	dec := xml.NewDecoder(bytes.NewReader(opf))
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("could not parse %s: %w", b.opfPath, err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "package":
			b.packageEl = rawStartElement(opf, "package")
		case "metadata":
			b.metadata = rawElement(opf, "metadata")
			b.readMetadataValues(b.metadata)
		case "item":
			b.items = append(b.items, manifestItem{
				ID:         attr(se, "id"),
				Href:       attr(se, "href"),
				MediaType:  attr(se, "media-type"),
				Properties: attr(se, "properties"),
			})
		case "itemref":
			if strings.EqualFold(attr(se, "linear"), "no") {
				continue // not part of the reading order
			}
			b.spineIDs = append(b.spineIDs, attr(se, "idref"))
		}
	}
	if len(b.items) == 0 {
		return fmt.Errorf("no manifest items in %s", b.opfPath)
	}
	if len(b.spineIDs) == 0 {
		return fmt.Errorf("no spine items in %s", b.opfPath)
	}
	if b.meta.Language == "" {
		b.meta.Language = "en"
	}
	return nil
}

var uniqueIDAttrRE = regexp.MustCompile(`unique-identifier\s*=\s*["']([^"']+)["']`)

// readMetadataValues extracts title, author, language and identifier from the
// raw <metadata> element.
func (b *epubBook) readMetadataValues(raw []byte) {
	uniqueID := ""
	if m := uniqueIDAttrRE.FindSubmatch(b.packageEl); m != nil {
		uniqueID = string(m[1])
	}
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false
	var (
		cur             string
		curID           string
		text            strings.Builder
		firstIdentifier string
	)
	flush := func() {
		value := squashSpace(text.String())
		text.Reset()
		switch cur {
		case "title":
			if b.meta.Title == "" {
				b.meta.Title = value
			}
		case "creator":
			if b.meta.Creator == "" {
				b.meta.Creator = value
			}
		case "language":
			if b.meta.Language == "" {
				b.meta.Language = value
			}
		case "identifier":
			if value == "" {
				return
			}
			if firstIdentifier == "" {
				firstIdentifier = value
			}
			if uniqueID != "" && curID == uniqueID {
				b.meta.Identifier = value
			}
		}
		cur = ""
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			cur = t.Name.Local
			curID = attr(t, "id")
			text.Reset()
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			if t.Name.Local == cur {
				flush()
			}
		}
	}
	if b.meta.Identifier == "" {
		b.meta.Identifier = firstIdentifier
	}
}

// fileLabel is the document name without directory and extension: the label a
// book without titles and without toc entries falls back to.
func fileLabel(zipPath string) string {
	base := path.Base(zipPath)
	return strings.TrimSuffix(base, path.Ext(base))
}

type tocEntry struct {
	Title string
	Href  string
	Depth int
}

// tocLabels maps every document referenced by the book's table of contents to
// its entry. A document can be referenced several times: the shallowest entry is
// the chapter, the deeper ones are the sections inside it (they point at the same
// document with a fragment).
func (b *epubBook) tocLabels(zr *zip.Reader) (map[string]tocEntry, error) {
	labels := map[string]tocEntry{}
	// A toc href is relative to the toc document, which need not live next to
	// the package.
	add := func(entries []tocEntry, baseDir string) {
		for _, e := range entries {
			key := joinHref(baseDir, e.Href)
			if cur, dup := labels[key]; !dup || e.Depth < cur.Depth {
				labels[key] = e
			}
		}
	}
	// EPUB3 nav document.
	if nav := b.navPath(); nav != "" {
		if raw, err := readZipEntry(zr, nav); err == nil {
			if entries, ok := parseNavTOC(raw); ok {
				add(entries, path.Dir(nav))
				return labels, nil
			}
		}
	}
	// EPUB2 NCX. A broken or missing toc costs the book its chapter titles, not
	// the audiobook: the headings inside the documents are the fallback.
	for _, it := range b.items {
		if it.MediaType != "application/x-dtbncx+xml" {
			continue
		}
		b.ncxPath = joinHref(b.opfDir, it.Href)
		raw, err := readZipEntry(zr, b.ncxPath)
		if err != nil {
			log.Printf("the table of contents of the epub (%s) cannot be read: %v", b.ncxPath, err)
			return labels, nil
		}
		entries, err := parseNCX(raw)
		if err != nil {
			log.Printf("the table of contents of the epub (%s) cannot be parsed: %v", b.ncxPath, err)
			return labels, nil
		}
		add(entries, path.Dir(b.ncxPath))
		break
	}
	return labels, nil
}

// ncxFrame is the state of one (possibly nested) navPoint while parsing toc.ncx.
type ncxFrame struct {
	label    strings.Builder
	inLabel  bool
	href     string
	captured bool
}

func top(stack []*ncxFrame) *ncxFrame {
	if len(stack) == 0 {
		return nil
	}
	return stack[len(stack)-1]
}

// parseNCX returns the navMap entries in play order, together with their nesting
// depth. A navPoint's own label and content come before its children, so the
// state has to be kept per navPoint: emitting on close without a stack would
// report the first section of a chapter as the chapter's title.
func parseNCX(raw []byte) ([]tocEntry, error) {
	var out []tocEntry
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false
	var stack []*ncxFrame
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("could not parse toc.ncx: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "navPoint":
				stack = append(stack, &ncxFrame{})
			case "navLabel":
				if f := top(stack); f != nil {
					f.inLabel = true
					f.label.Reset()
				}
			case "content":
				if f := top(stack); f != nil {
					f.href = attr(t, "src")
					f.captured = f.href != ""
				}
			}
		case xml.CharData:
			if f := top(stack); f != nil && f.inLabel {
				f.label.Write(t)
			}
		case xml.EndElement:
			if t.Name.Local == "navLabel" {
				if f := top(stack); f != nil {
					f.inLabel = false
				}
				continue
			}
			if t.Name.Local != "navPoint" || len(stack) == 0 {
				continue
			}
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			title := squashSpace(f.label.String())
			if f.captured && title != "" {
				out = append(out, tocEntry{Title: title, Href: f.href, Depth: len(stack) + 1})
			}
		}
	}
	return out, nil
}

// parseNavTOC extracts the entries of the <nav epub:type="toc"> list of an EPUB3
// nav document. The second return value reports whether a toc nav was present.
func parseNavTOC(raw []byte) ([]tocEntry, bool) {
	doc, err := nethtml.Parse(bytes.NewReader(raw))
	if err != nil {
		return nil, false
	}
	var (
		out   []tocEntry
		isTOC bool
		walk  func(n *nethtml.Node, inTOC bool, depth int)
	)
	walk = func(n *nethtml.Node, inTOC bool, depth int) {
		if n.Type == nethtml.ElementNode {
			here := inTOC
			if n.Data == "nav" && roleSet(n)["toc"] {
				isTOC = true
				here = true
			}
			if here && n.Data == "ol" {
				depth++
			}
			if n.Data == "a" && here {
				href := htmlAttr(n, "href")
				text := squashSpace(nodeText(n))
				if href != "" && text != "" {
					out = append(out, tocEntry{Title: text, Href: href, Depth: max(depth, 1)})
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c, here, depth)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inTOC, depth)
		}
	}
	walk(doc, false, 0)
	return out, isTOC
}

type htmlAnalysis struct {
	Text      string
	Heading   string
	DocTitle  string
	Roles     map[string]bool
	LinkCount int
	LinkChars int
}

// analyseHTML extracts the spoken text (audiblez's tag set), the first heading,
// the <title>, the EPUB3 roles and how much of the text is link text.
func analyseHTML(raw []byte) htmlAnalysis {
	a := htmlAnalysis{Roles: map[string]bool{}}
	doc, err := nethtml.Parse(bytes.NewReader(raw))
	if err != nil {
		return a
	}
	var blocks []string
	var walk func(n *nethtml.Node)
	walk = func(n *nethtml.Node) {
		if n.Type == nethtml.ElementNode {
			if n.Data == "script" || n.Data == "style" || n.Data == "head" {
				return
			}
			for _, r := range strings.Fields(epubTypeAttr(n)) {
				a.Roles[r] = true
			}
			if n.Data == "a" && htmlAttr(n, "href") != "" {
				if t := squashSpace(nodeText(n)); t != "" {
					a.LinkCount++
					a.LinkChars += len(t)
				}
			}
			if htmlTextTags[n.Data] {
				t := squashSpace(nodeText(n))
				if t != "" {
					blocks = append(blocks, t)
					if a.Heading == "" && strings.HasPrefix(n.Data, "h") {
						a.Heading = t
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if t := firstElementText(doc, "title"); t != "" {
		a.DocTitle = t
	}
	a.Text = strings.Join(blocks, "\n")
	return a
}

// --- chapter selection ----------------------------------------------------

// chapterFilter decides which spine documents become audiobook chapters.
type chapterFilter struct {
	// MinTextChars: a document without a TOC label or heading is only kept when
	// it carries at least this much text and follows a kept document.
	MinTextChars int
	// SkipPatterns are matched case insensitively against the chapter title.
	SkipPatterns []*regexp.Regexp
	// KeepPatterns win over SkipPatterns.
	KeepPatterns []*regexp.Regexp
}

// selectedChapters returns the readable chapters in reading order.
func (b *epubBook) selectedChapters(f chapterFilter) []epubChapter {
	var out []epubChapter
	for _, ch := range b.spine {
		keep, reason := b.keepChapter(ch, f)
		if keep {
			out = append(out, *ch)
			continue
		}
		// The reason belongs in the log: "why is chapter X missing" is the
		// question this answers.
		log.Printf("  skipping '%s' (%d characters): %s", ch.Title, ch.charCount(), reason)
	}
	return out
}

// keepChapter applies the keep patterns, then the structural rules, then the
// title rules.
func (b *epubBook) keepChapter(ch *epubChapter, f chapterFilter) (bool, string) {
	// An explicit keep pattern wins over everything below: it is how a real
	// chapter called "Index of Translated Works" survives the built-in list.
	if matchAny(f.KeepPatterns, ch.Title) {
		return true, "keep pattern"
	}
	if ch.looksLikeTOC() {
		return false, "table of contents"
	}
	for role := range ch.Roles {
		if nonContentRoles[role] {
			return false, "epub:type=" + role
		}
	}
	if ch.charCount() < audiblezMinChapterChars {
		return false, "too little text"
	}
	if matchAny(f.SkipPatterns, ch.Title) {
		return false, "title matches skip pattern"
	}
	if ch.named() {
		return true, "named chapter"
	}
	if hasAnyRole(ch, contentRoles) {
		return true, "content role"
	}
	// Untitled document: no toc entry, no heading, no content role. If the book
	// has a table of contents, it did not mention this document, so it is a
	// fragment (a calibre split of another file) or filler. Without any toc
	// there is nothing to go by except length, and dropping the first chapter of
	// a novel would drop every chapter after it too.
	if b.hasTOC {
		return false, "not in the table of contents"
	}
	if ch.charCount() >= f.MinTextChars {
		return true, "untitled but substantial"
	}
	return false, "untitled"
}

func hasAnyRole(c *epubChapter, roles map[string]bool) bool {
	for r := range c.Roles {
		if roles[r] {
			return true
		}
	}
	return false
}

func matchAny(pats []*regexp.Regexp, s string) bool {
	for _, re := range pats {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// --- derived epub ---------------------------------------------------------

// chapterFileName builds the canonical name for chapter number index (1 based)
// of total: "chapter_01.xhtml", kept in the directory of the original document
// so that relative stylesheet and image references still resolve.
func chapterFileName(origZipPath string, index, total int) string {
	width := 2
	if d := len(strconv.Itoa(total)); d > width {
		width = d
	}
	ext := path.Ext(origZipPath)
	if ext == "" {
		ext = ".xhtml"
	}
	name := fmt.Sprintf("chapter_%0*d%s", width, index, ext)
	if dir := path.Dir(origZipPath); dir != "." && dir != "" {
		return dir + "/" + name
	}
	return name
}

// writeDerivedEpub writes an epub holding only the given chapters, named
// chapter_01.xhtml ... in reading order, with manifest and spine in that same
// order, plus the cover image and the non-image assets. Everything else is
// dropped: images are irrelevant for speech synthesis.
func (b *epubBook) writeDerivedEpub(dest string, chapters []epubChapter) ([]string, error) {
	zr, err := zip.OpenReader(b.path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	out, err := os.Create(dest)
	if err != nil {
		return nil, fmt.Errorf("could not create %s: %w", dest, err)
	}
	zw := zip.NewWriter(out)
	fail := func(err error) ([]string, error) {
		zw.Close()
		out.Close()
		os.Remove(dest)
		return nil, err
	}

	write := func(name string, data []byte, method uint16) error {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method})
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}

	// 1. mimetype: first entry, stored.
	mimetype := []byte("application/epub+zip")
	if raw, err := readZipEntry(&zr.Reader, "mimetype"); err == nil && len(raw) > 0 {
		mimetype = raw
	}
	if err := write("mimetype", mimetype, zip.Store); err != nil {
		return fail(err)
	}

	// 2. chapters under their canonical names.
	names := make([]string, len(chapters))
	for i, ch := range chapters {
		names[i] = chapterFileName(ch.ZipPath, i+1, len(chapters))
	}
	for i, ch := range chapters {
		src, err := readZipEntry(&zr.Reader, ch.ZipPath)
		if err != nil {
			return fail(err)
		}
		if err := write(names[i], src, zip.Deflate); err != nil {
			return fail(err)
		}
	}

	// 3. assets (style sheets, fonts, cover) and META-INF.
	keptPath := make(map[string]bool, len(chapters))
	for _, ch := range chapters {
		keptPath[ch.ZipPath] = true
	}
	// A stray member of the zip that carries one of the canonical names (and is
	// not referenced by the package) must not be copied over the chapter we
	// generated under that name: a zip with two entries of one name is broken.
	generated := make(map[string]bool, len(names))
	for _, n := range names {
		generated[n] = true
	}
	ncxPath := b.ncxPath
	if ncxPath == "" {
		ncxPath = path.Join(b.opfDir, "toc.ncx")
	}
	regenerated := map[string]bool{
		"mimetype":  true,
		b.opfPath:   true,
		ncxPath:     true,
		b.navPath(): true,
	}
	for _, f := range zr.File {
		if regenerated[f.Name] || keptPath[f.Name] || generated[f.Name] {
			continue
		}
		if item, ok := b.itemByPath(f.Name); ok {
			if isDocument(item) {
				continue // skipped front/back matter, or a renamed chapter
			}
			if strings.HasPrefix(item.MediaType, "image/") && item.ID != b.coverID {
				continue
			}
		}
		src, err := func() ([]byte, error) {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}()
		if err != nil {
			return fail(err)
		}
		if err := write(f.Name, src, f.Method); err != nil {
			return fail(err)
		}
	}

	// 4. generated NCX (the derived book stays navigable) and package document.
	ncx, err := b.renderNCX(chapters, names, ncxPath)
	if err != nil {
		return fail(err)
	}
	if err := write(ncxPath, ncx, zip.Deflate); err != nil {
		return fail(err)
	}
	opf, err := b.renderOPF(chapters, names, ncxPath)
	if err != nil {
		return fail(err)
	}
	if err := write(b.opfPath, opf, zip.Deflate); err != nil {
		return fail(err)
	}
	if err := zw.Close(); err != nil {
		out.Close()
		os.Remove(dest)
		return nil, err
	}
	return names, out.Close()
}

// navPath is the zip path of the EPUB3 nav document, or "".
func (b *epubBook) navPath() string {
	for _, it := range b.items {
		if strings.Contains(it.Properties, "nav") {
			return joinHref(b.opfDir, it.Href)
		}
	}
	return ""
}

// renderOPF reuses the original <package> tag and <metadata> element and writes
// a manifest and spine listing the kept chapters in reading order.
func (b *epubBook) renderOPF(chapters []epubChapter, names []string, ncxPath string) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	if len(b.packageEl) > 0 {
		buf.Write(b.packageEl)
	} else {
		buf.WriteString(`<package xmlns="http://www.idpf.org/2007/opf" version="2.0" unique-identifier="id">`)
	}
	buf.WriteByte('\n')
	if len(b.metadata) > 0 {
		buf.Write(b.metadata)
		buf.WriteByte('\n')
	} else {
		fmt.Fprintf(&buf, "<metadata xmlns:dc=\"http://purl.org/dc/elements/1.1/\"><dc:title>%s</dc:title></metadata>\n",
			xmlEscape(b.meta.Title))
	}
	buf.WriteString("<manifest>\n")
	fmt.Fprintf(&buf, "  <item id=\"ncx\" href=\"%s\" media-type=\"application/x-dtbncx+xml\"/>\n",
		xmlEscape(relHref(b.opfDir, ncxPath)))
	for i, name := range names {
		// The manifest hrefs are relative to the package document, not to the
		// root of the container.
		fmt.Fprintf(&buf, "  <item id=\"chapter%02d\" href=\"%s\" media-type=\"application/xhtml+xml\"/>\n",
			i+1, xmlEscape(relHref(b.opfDir, name)))
	}
	if item, ok := b.item(b.coverID); ok {
		attrs := fmt.Sprintf(" id=\"%s\" href=\"%s\" media-type=\"%s\"",
			xmlEscape(item.ID), xmlEscape(item.Href), xmlEscape(item.MediaType))
		// properties="cover-image" has to survive or an EPUB3 reader loses it.
		if item.Properties != "" {
			attrs += fmt.Sprintf(" properties=\"%s\"", xmlEscape(item.Properties))
		}
		fmt.Fprintf(&buf, "  <item%s/>\n", attrs)
	}
	for _, it := range b.items {
		if isDocument(it) || it.ID == b.coverID || it.ID == "ncx" ||
			it.MediaType == "application/x-dtbncx+xml" ||
			strings.Contains(it.Properties, "nav") {
			continue
		}
		if strings.HasPrefix(it.MediaType, "image/") {
			continue // images are irrelevant for speech synthesis
		}
		attrs := fmt.Sprintf(" id=\"%s\" href=\"%s\" media-type=\"%s\"",
			xmlEscape(it.ID), xmlEscape(it.Href), xmlEscape(it.MediaType))
		if it.Properties != "" {
			attrs += fmt.Sprintf(" properties=\"%s\"", xmlEscape(it.Properties))
		}
		fmt.Fprintf(&buf, "  <item%s/>\n", attrs)
	}
	buf.WriteString("</manifest>\n<spine toc=\"ncx\">\n")
	for i := range chapters {
		fmt.Fprintf(&buf, "  <itemref idref=\"chapter%02d\"/>\n", i+1)
	}
	buf.WriteString("</spine>\n</package>\n")
	return buf.Bytes(), nil
}

// renderNCX writes a minimal EPUB2 navMap carrying the chapter titles, so the
// derived epub exposes the same titles and order as the audio files.
func (b *epubBook) renderNCX(chapters []epubChapter, names []string, ncxPath string) ([]byte, error) {
	uid := b.meta.Identifier
	if uid == "" {
		uid = "urn:uuid:audiobookconverter"
	}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	fmt.Fprintf(&buf, "<ncx xmlns=\"http://www.daisy.org/z3986/2005/ncx/\" version=\"2005-1\" xml:lang=\"%s\">\n",
		xmlEscape(b.meta.Language))
	buf.WriteString("<head>\n")
	fmt.Fprintf(&buf, "  <meta name=\"dtb:uid\" content=\"%s\"/>\n", xmlEscape(uid))
	buf.WriteString("  <meta name=\"dtb:depth\" content=\"1\"/>\n")
	buf.WriteString("  <meta name=\"dtb:generator\" content=\"audiobookconverter\"/>\n")
	buf.WriteString("</head>\n<docTitle><text>")
	buf.Write(xmlEscapeBytes(b.meta.Title))
	buf.WriteString("</text></docTitle>\n<navMap>\n")
	ncxDir := path.Dir(ncxPath)
	for i, ch := range chapters {
		fmt.Fprintf(&buf, "  <navPoint id=\"chapter%02d\" playOrder=\"%d\"><navLabel><text>", i+1, i+1)
		buf.Write(xmlEscapeBytes(ch.Title))
		fmt.Fprintf(&buf, "</text></navLabel><content src=\"%s\"/></navPoint>\n",
			xmlEscape(relHref(ncxDir, names[i])))
	}
	buf.WriteString("</navMap>\n</ncx>\n")
	return buf.Bytes(), nil
}

func (b *epubBook) detectCover() string {
	for _, it := range b.items {
		if strings.Contains(it.Properties, "cover-image") {
			return it.ID
		}
	}
	re := regexp.MustCompile(`(?i)<meta[^>]+name=["']cover["'][^>]+content=["']([^"']+)["']`)
	if m := re.FindSubmatch(b.metadata); m != nil {
		return string(m[1])
	}
	for _, it := range b.items {
		if strings.HasPrefix(it.MediaType, "image/") && strings.Contains(strings.ToLower(it.ID), "cover") {
			return it.ID
		}
	}
	return ""
}

func (b *epubBook) item(id string) (manifestItem, bool) {
	if id == "" {
		return manifestItem{}, false
	}
	for _, it := range b.items {
		if it.ID == id {
			return it, true
		}
	}
	return manifestItem{}, false
}

func (b *epubBook) itemByPath(zipPath string) (manifestItem, bool) {
	for _, it := range b.items {
		if joinHref(b.opfDir, it.Href) == zipPath {
			return it, true
		}
	}
	return manifestItem{}, false
}

// --- helpers --------------------------------------------------------------

func isDocument(it manifestItem) bool {
	return it.MediaType == "application/xhtml+xml" || it.MediaType == "text/html"
}

// relHref expresses a zip path as an href relative to dir. Manifest and navMap
// hrefs are resolved against the directory of their own document, not against
// the root of the container, so a book whose package sits in OEBPS must not be
// told to look for OEBPS/OEBPS/chapter_01.xhtml. Documents outside that
// directory (a Text/ folder next to OEBPS/) are reached with "..".
func relHref(dir, zipPath string) string {
	if dir == "" || dir == "." {
		return zipPath
	}
	// Climb out of dir up to the directory the two paths share, then walk down
	// again: "OEBPS" -> "Text/x.xhtml" is "../Text/x.xhtml", "A/B" ->
	// "A/C/x.xhtml" is "../C/x.xhtml".
	dirs := strings.Split(dir, "/")
	parts := strings.Split(zipPath, "/")
	shared := 0
	for shared < len(dirs) && shared < len(parts)-1 && dirs[shared] == parts[shared] {
		shared++
	}
	return strings.Repeat("../", len(dirs)-shared) + strings.Join(parts[shared:], "/")
}

// joinHref resolves an href against the directory of the document it comes from
// (the package for manifest hrefs, the toc for navMap hrefs) and normalises it
// into a zip entry name. A leading "/" is the root of the container, and hrefs
// may be percent encoded.
func joinHref(baseDir, href string) string {
	href = strings.SplitN(href, "#", 2)[0]
	href = strings.SplitN(href, "?", 2)[0]
	if dec, err := url.PathUnescape(href); err == nil {
		href = dec
	}
	href = strings.ReplaceAll(href, "\\/", "/")
	if root := strings.TrimPrefix(href, "/"); root != href {
		return path.Clean(root)
	}
	href = strings.TrimPrefix(href, "./")
	if baseDir == "" || baseDir == "." {
		return path.Clean(href)
	}
	return path.Clean(path.Join(baseDir, href))
}

func readZipEntry(zr *zip.Reader, name string) ([]byte, error) {
	if name == "" {
		return nil, fmt.Errorf("empty entry name")
	}
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("%s not found in epub", name)
}

func rootfilePath(zr *zip.Reader) (string, error) {
	raw, err := readZipEntry(zr, "META-INF/container.xml")
	if err != nil {
		return "", err
	}
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false
	first := ""
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("could not parse META-INF/container.xml: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "rootfile" {
			continue
		}
		p := attr(se, "full-path")
		if p == "" {
			continue
		}
		if first == "" {
			first = p
		}
		if strings.Contains(attr(se, "media-type"), "package") {
			return path.Clean(p), nil
		}
	}
	if first == "" {
		return "", fmt.Errorf("no rootfile in META-INF/container.xml")
	}
	return path.Clean(first), nil
}

func attr(se xml.StartElement, name string) string {
	for _, a := range se.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func firstElementText(n *nethtml.Node, tag string) string {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == nethtml.ElementNode {
			if c.Data == tag {
				return squashSpace(nodeText(c))
			}
			if t := firstElementText(c, tag); t != "" {
				return t
			}
		}
	}
	return ""
}

// nodeText concatenates the text of a subtree, skipping script and style.
func nodeText(n *nethtml.Node) string {
	var sb strings.Builder
	var walk func(*nethtml.Node)
	walk = func(c *nethtml.Node) {
		if c.Type == nethtml.ElementNode && (c.Data == "script" || c.Data == "style") {
			return
		}
		if c.Type == nethtml.TextNode {
			sb.WriteString(c.Data)
		}
		for ch := c.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(n)
	return sb.String()
}

func htmlAttr(n *nethtml.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

// rawStartElement returns the verbatim "<name ...>" start tag of the first
// element with that name, or nil. Raw byte scanning is used on purpose: it keeps
// the namespace declarations of the original package document intact and does not
// depend on the shape of xml.Decoder.RawToken, which changed in recent Go
// versions.
func rawStartElement(src []byte, name string) []byte {
	open := tagStart(src, name)
	if open < 0 {
		return nil
	}
	inQuote := byte(0)
	for i := open + 1; i < len(src); i++ {
		switch {
		case inQuote != 0:
			if src[i] == inQuote {
				inQuote = 0
			}
		case src[i] == '"' || src[i] == '\'':
			inQuote = src[i]
		case src[i] == '>':
			out := make([]byte, i-open+1)
			copy(out, src[open:i+1])
			return out
		}
	}
	return nil
}

// rawElement returns the verbatim element including its children. Nested
// elements of the same name are not expected inside <metadata>.
func rawElement(src []byte, name string) []byte {
	open := tagStart(src, name)
	if open < 0 {
		return nil
	}
	closeTag := []byte("</" + name)
	if i := indexBytesFrom(src, closeTag, open); i >= 0 {
		end := i + len(closeTag)
		if j := indexByteFrom(src, '>', end); j >= 0 {
			out := make([]byte, j-open+1)
			copy(out, src[open:j+1])
			return out
		}
	}
	return nil
}

// tagStart returns the offset of "<name" when it is followed by whitespace or
// the end of the tag, so that <metadata does not match <metadataFoo.
func tagStart(src []byte, name string) int {
	needle := []byte("<" + name)
	for from := 0; ; {
		i := indexBytesFrom(src, needle, from)
		if i < 0 {
			return -1
		}
		next := i + len(needle)
		if next < len(src) {
			switch c := src[next]; c {
			case ' ', '\t', '\n', '\r', '>', '/':
				return i
			}
			from = next
			continue
		}
		return i
	}
}

func indexBytesFrom(src, needle []byte, from int) int {
	if from >= len(src) {
		return -1
	}
	if i := bytes.Index(src[from:], needle); i >= 0 {
		return from + i
	}
	return -1
}

func indexByteFrom(src []byte, b byte, from int) int {
	if from >= len(src) {
		return -1
	}
	if i := bytes.IndexByte(src[from:], b); i >= 0 {
		return from + i
	}
	return -1
}

// epubTypeAttr returns the EPUB3 structure type of an element. The html parser
// keeps the prefix, but a plain type attribute is not an epub:type.
func epubTypeAttr(n *nethtml.Node) string {
	return htmlAttr(n, "epub:type")
}

func roleSet(n *nethtml.Node) map[string]bool {
	out := map[string]bool{}
	for _, r := range strings.Fields(epubTypeAttr(n)) {
		out[r] = true
	}
	return out
}

// squashSpace collapses whitespace runs into single spaces.
func squashSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func xmlEscape(s string) string { return string(xmlEscapeBytes(s)) }

func xmlEscapeBytes(s string) []byte {
	var buf bytes.Buffer
	if err := xml.EscapeText(&buf, []byte(s)); err != nil {
		return []byte(s)
	}
	return buf.Bytes()
}
