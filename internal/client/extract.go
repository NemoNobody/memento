package client

import (
	"html"
	"strings"
	"unicode"
	"unicode/utf8"

	xhtml "golang.org/x/net/html"

	"github.com/NemoNobody/memento/internal/model"
)

// Extraction limits. A page is research input, not a target: the stored text
// must fit in an agent's context, so extraction caps sections, section size,
// and total size. Truncation is reported, never silent.
const (
	MaxSections     = 16
	MaxSectionChars = 4000
	MaxDocChars     = 24000
	MaxTitleChars   = 200
	maxHeadingChars = 120
)

// The page is parsed with the HTML5 tokenizer (golang.org/x/net/html), the same
// one the Go ecosystem and browsers agree on. A hand-rolled scanner was tried
// first and produced a steady stream of bug classes — tags inside comments,
// quotes inside attributes, "<" in prose, CRLF separators, unclosed elements —
// each one a silent difference from what a reader sees. The tree costs one
// parse per page and removes that class outright.

// dropElements are removed with everything inside them: they carry scripts and
// styling, not readable text. head is dropped because the title is taken
// separately (ExtractTitle) and the rest (meta, link, style) is noise.
var dropElements = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true,
	"svg": true, "select": true, "option": true, "head": true,
}

// chromeElements are page furniture: a link rail is not documentation. They are
// dropped unless the markup is so broken that they hold most of the page (see
// chromeDominates) — an unclosed <nav> swallows the rest of the page in the
// tree, and deleting that would store an empty document.
var chromeElements = map[string]bool{"nav": true, "aside": true, "footer": true}

// headingElements open a new section. Deep headings (h5/h6) are treated as
// plain text: a section tree is not the goal, a handful of quotable chunks is.
var headingElements = map[string]bool{"h1": true, "h2": true, "h3": true, "h4": true}

// blockElements end a text line, approximating block layout.
var blockElements = map[string]bool{
	"p": true, "div": true, "li": true, "ul": true, "ol": true,
	"table": true, "tr": true, "td": true, "th": true, "pre": true,
	"blockquote": true, "section": true, "article": true, "header": true,
	"footer": true, "nav": true, "aside": true, "dd": true, "dt": true,
	"form": true, "figure": true, "figcaption": true, "hr": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
}

// lowerASCII lowercases ASCII A-Z bytes and leaves every other byte untouched,
// so the result is always byte-length-equal to the input. strings.ToLower is
// unsafe for the body/lower index sharing in ExtractTitle: Go's Unicode
// lowering changes byte length (U+2126 OHM SIGN shrinks 3 to 2 bytes, U+023A
// grows 2 to 3), which desynchronizes shared byte indices and panics on slice
// bounds with ordinary prose (an "Ω" in a technical article is enough).
func lowerASCII(s string) string {
	need := false
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			need = true
			break
		}
	}
	if !need {
		return s
	}
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// ExtractTitle returns the text of the <title> element, capped. Empty when the
// page has no title. It scans raw bytes rather than the parsed tree: the title is
// wanted on its own (fetch reports it even when extraction is unhelpful), and a
// second pass over the page is cheaper than threading a tree through the caller.
// Comments and raw-text elements are skipped in the same pass, so a "<title>"
// inside either is not mistaken for the page title. One pass, no look-back: the
// scanner used to ask "is this offset inside a comment" from the start of the
// document on every hit, which is quadratic on a page full of "<title>".
func ExtractTitle(body string) string {
	lower := lowerASCII(body)
	for i := 0; i < len(lower); {
		lt := strings.IndexByte(lower[i:], '<')
		if lt < 0 {
			return ""
		}
		p := i + lt
		if strings.HasPrefix(lower[p:], "<![cdata[") {
			// In HTML content this is a bogus comment: it ends at the next '>'.
			j := strings.IndexByte(lower[p:], '>')
			if j < 0 {
				return ""
			}
			i = p + j + 1
			continue
		}
		if strings.HasPrefix(lower[p:], "<!--") {
			rest := lower[p+4:]
			switch {
			case strings.HasPrefix(rest, ">"): // "<!-->" closes immediately
				i = p + 5
			case strings.HasPrefix(rest, "->"):
				i = p + 6
			default:
				// One scan to the nearest terminator ("-->", or the legacy
				// "--!>"). Looking for the two forms with separate searches made
				// the pass quadratic on a page made of comments.
				end := commentEnd(lower, p+4)
				if end < 0 {
					return ""
				}
				i = end
			}
			continue
		}
		name, isClose, end := tagName(lower, p)
		if name == "" {
			i = end
			continue
		}
		switch {
		case !isClose && name == "title":
			close := findTagStartFrom(lower, "/title", end)
			if close < 0 {
				// A document cut inside the title still has a title as far as a
				// reader is concerned.
				return capRunes(cleanText(body[end:]), MaxTitleChars)
			}
			return capRunes(cleanText(body[end:close]), MaxTitleChars)
		case !isClose && opaqueElements[name] && !opaqueSelfClosing(name, lower[p:end]):
			close := findTagStartFrom(lower, "/"+name, end)
			if close < 0 {
				return ""
			}
			i = close + len(name) + 2 // past "</name"
			continue
		}
		i = end
	}
	return ""
}

// tagName reads the element name at pos, whether this is a closing tag, and the
// offset just past '>' (or the end of the document). An empty name means the
// "<" is not markup. Quoted attribute values are skipped: a ">" inside one does
// not end the tag, and markup inside one is not markup.
func tagName(lower string, pos int) (name string, isClose bool, end int) {
	k := pos + 1
	if k < len(lower) && lower[k] == '/' {
		isClose = true
		k++
	}
	start := k
	for k < len(lower) && (isAlpha(lower[k]) || isDigit(lower[k])) {
		k++
	}
	if k == start {
		return "", false, pos + 1
	}
	return lower[start:k], isClose, tagEnd(lower, k)
}

// opaqueSelfClosing reports whether a self-closing slash closes an opaque
// element. HTML ignores the slash outside foreign content, so <script/> is an
// open script (and its raw text runs to </script>), while <svg/> is closed.
func opaqueSelfClosing(name, tag string) bool {
	return (name == "svg" || name == "math") && selfClosingTag(tag)
}

// commentEnd returns the offset just past the terminator of the comment whose
// body starts at from, or -1 when the comment never closes. One left-to-right
// scan: "-->" and "--!>" are both matched at the same marker.
func commentEnd(lower string, from int) int {
	for i := from; i+2 < len(lower); i++ {
		if lower[i] != '-' || lower[i+1] != '-' {
			continue
		}
		j := i + 2
		if lower[j] == '!' {
			j++
		}
		if j < len(lower) && lower[j] == '>' {
			return j + 1
		}
	}
	return -1
}

// selfClosingTag reports whether the tag text ends with "/>".
func selfClosingTag(tag string) bool {
	return strings.HasSuffix(strings.TrimSpace(tag), "/>")
}

// tagEnd returns the offset just past the '>' that ends a tag, ignoring '>'
// inside quoted attribute values. A quote only opens a value right after '='
// (with optional spaces): an apostrophe in an unquoted value ("title=don't")
// is part of the value, not the start of a quoted one.
func tagEnd(lower string, from int) int {
	for i := from; i < len(lower); i++ {
		switch c := lower[i]; c {
		case '"', '\'':
			j := i - 1
			for j >= from && (lower[j] == ' ' || lower[j] == '\t' || lower[j] == '\n' || lower[j] == '\r') {
				j--
			}
			if j < from || lower[j] != '=' {
				continue
			}
			end := strings.IndexByte(lower[i+1:], c)
			if end < 0 {
				return len(lower)
			}
			i += 1 + end
		case '>':
			return i + 1
		}
	}
	return len(lower)
}

func isAlpha(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }
func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// ExtractSections splits page HTML into heading-delimited text sections and
// collects referenced images (url+alt+title; binaries are never stored). It
// reports whether the limits truncated the content.
//
// The extraction prefers the page's main-content container when the markup
// names one (<main>, role="main", a lone <article>): sidebar links and footers
// are not documentation, and a rail that comes first in the source would eat
// the document budget before the article starts. A container that yields
// nothing falls through to the next candidate and finally to the whole page.
func ExtractSections(body string) (sections []model.Section, images []model.ImageRef, truncated bool) {
	doc, err := xhtml.Parse(strings.NewReader(body))
	if err != nil {
		// The tree parser refuses markup nested deeper than 512 open elements
		// (its own stack cap). Text is still worth storing: sections are lost,
		// the page is not. A page that is nothing but chrome is retried with the
		// chrome kept, same as on the tree path.
		secs, imgs, tr := extractTextOnly(body, false)
		if len(secs) == 0 {
			secs, imgs, tr = extractTextOnly(body, true)
		}
		return secs, imgs, tr
	}
	page := findNode(doc, func(n *xhtml.Node) bool { return n.Data == "body" })
	if page == nil {
		page = doc
	}
	keepChrome := chromeDominates(page)
	for _, root := range contentRoots(page) {
		e := &extractor{keepChrome: keepChrome}
		e.walk(root, nil)
		e.flush()
		secs, tr := buildSections(e.raw, true, e.truncated)
		if len(secs) == 0 {
			// A root that is nothing but link blocks is not worth losing the page
			// over: keep its sections, chrome filter off.
			secs, tr = buildSections(e.raw, false, e.truncated)
		}
		if len(secs) > 0 {
			return secs, e.images, tr
		}
	}
	e := &extractor{keepChrome: keepChrome}
	e.walk(page, nil)
	e.flush()
	secs, tr := buildSections(e.raw, true, e.truncated)
	if len(secs) == 0 && len(e.raw) > 0 {
		// A page that is nothing but link blocks is still a page: dropping every
		// section would store an empty document and hide the loss.
		secs, tr = buildSections(e.raw, false, e.truncated)
	}
	return secs, e.images, tr
}

// piece is one run of section text: inline text is whitespace-collapsed and
// junk-filtered, <pre> text is kept verbatim (newlines and indentation), which
// is the whole point of a code block.
type piece struct {
	text     string
	verbatim bool
}

// linkState tracks one <a href>: the section it opened in, and whether its text
// has already been counted as a link in that section. Counting on open is
// wrong: the parser leaves empty anchors behind (an unclosed link is
// re-opened per block), and an empty anchor is not a menu entry.
type linkState struct {
	section int
	counted bool
}

// extractor accumulates raw sections while walking one content root.
type extractor struct {
	keepChrome bool

	raw       []rawSection
	images    []model.ImageRef
	truncated bool

	heading string
	pieces  []piece
	// Link arithmetic runs on LETTERS, not on all visible runes: separators
	// (">", "/", "|", ",", "::") carry none, so "no letter outside a link" is
	// exactly the shape of a rail, while any word outside one means prose.
	inlineLetters int
	linkLetters   int
	anchors       int  // links with text in the current section
	hasPre        bool // the section carries verbatim code: never a rail
	blank         bool // a block boundary happened; the next text starts a line
	seen          bool
	section       int // index of the section being built
}

// walk collects text, images and section boundaries from n. link is the
// enclosing <a href>, if any: text counts as link text only when the link
// opened in the section being built, so an unclosed anchor cannot turn the next
// section into a rail. Inside its own section an unclosed link still reads as
// link text — the parser re-opens it per block, exactly as a browser does, and
// that is a known limit rather than something the walker can guess around.
func (e *extractor) walk(n *xhtml.Node, link *linkState) {
	childLink := link // an <a> applies to its own subtree, not to its siblings
	switch n.Type {
	case xhtml.TextNode:
		e.writeText(n.Data, link)
		return
	case xhtml.ElementNode:
		name := n.Data
		if dropElements[name] {
			return
		}
		if chromeElements[name] && !e.keepChrome {
			return
		}
		switch {
		case name == "img":
			e.addImage(n)
			return
		case headingElements[name]:
			e.flush()
			e.heading = capRunes(nodeText(n), maxHeadingChars)
			return
		case name == "pre":
			e.writePre(n)
			return
		case name == "br":
			e.blank = e.seen
		case name == "a":
			// Only a real link counts: <a id="anchor"> or <a name="x"> is an
			// anchor target, and the parser keeps it open to the end of its
			// parent, so counting that text as link text would drop real prose.
			if href := attr(n, "href"); href != "" && !wrapsBlock(n) {
				childLink = &linkState{section: e.section}
			}
		default:
			if blockElements[name] {
				e.blank = e.seen
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		e.walk(c, childLink)
	}
	if n.Type == xhtml.ElementNode {
		// An element's close ends the line too: <div>one<div>two</div>three</div>
		// must not read as "twothree" (h5 and other text-level elements are
		// blocks here without opening a section).
		if blockElements[n.Data] {
			e.blank = e.seen
		}
	}
}

func (e *extractor) writeText(raw string, link *linkState) {
	s := inlineText(raw)
	if s == "" {
		return
	}
	if e.blank {
		e.pieces = append(e.pieces, piece{text: "\n"})
	}
	e.blank = false
	e.seen = true
	letters := lettersIn(s)
	e.inlineLetters += letters
	if link != nil && link.section == e.section && letters > 0 {
		e.linkLetters += letters
		if !link.counted {
			link.counted = true
			e.anchors++
		}
	}
	// The text is stored as written (minus control bytes, newlines turned into
	// spaces): inline elements split a sentence into many text nodes, and
	// collapsing each node on its own would glue "simple" to "if" and then
	// drop the short word as junk.
	e.pieces = append(e.pieces, piece{text: s})
}

func (e *extractor) writePre(n *xhtml.Node) {
	text := preText(n)
	if text == "" {
		return
	}
	if e.blank && e.seen {
		e.pieces = append(e.pieces, piece{text: "\n"})
	}
	e.blank = true
	e.seen = true
	e.hasPre = true
	e.pieces = append(e.pieces, piece{text: text, verbatim: true})
}

func (e *extractor) addImage(n *xhtml.Node) {
	src := strings.TrimSpace(attr(n, "src"))
	if src == "" {
		src = strings.TrimSpace(attr(n, "data-src"))
	}
	if src == "" || strings.HasPrefix(strings.ToLower(src), "data:") {
		return
	}
	if len(e.images) >= maxImages {
		// The flag reports any loss, images included: a fetch answer that says
		// nothing was dropped must not be lying.
		e.truncated = true
		return
	}
	e.images = append(e.images, model.ImageRef{
		URL:   src,
		Alt:   collapseText(attr(n, "alt")),
		Title: collapseText(attr(n, "title")),
	})
}

// flush closes the current section and starts a fresh one.
func (e *extractor) flush() {
	e.raw = append(e.raw, rawSection{
		heading:       e.heading,
		text:          e.render(),
		inlineLetters: e.inlineLetters,
		linkLetters:   e.linkLetters,
		anchors:       e.anchors,
		hasPre:        e.hasPre,
	})
	e.heading = ""
	e.pieces = nil
	e.inlineLetters = 0
	e.linkLetters = 0
	e.anchors = 0
	e.hasPre = false
	e.blank = false
	e.seen = false
	e.section++
}

// render turns the collected pieces into section text: inline runs are joined
// and folded into lines (whitespace collapsed, junk filtered), verbatim runs are
// kept as written.
func (e *extractor) render() string {
	var lines []string
	var inline strings.Builder
	flushInline := func() {
		for _, line := range strings.Split(inline.String(), "\n") {
			line = collapseText(line)
			if line == "" || junkLine(line) {
				continue
			}
			lines = append(lines, line)
		}
		inline.Reset()
	}
	for _, p := range e.pieces {
		if p.verbatim {
			flushInline()
			lines = append(lines, strings.Split(p.text, "\n")...)
			continue
		}
		inline.WriteString(p.text)
	}
	flushInline()
	return strings.Join(lines, "\n")
}

// rawSection is one heading-delimited chunk: its heading, its text, and what the
// chrome heuristic needs — how many letters sit in inline text and how many of
// them are inside links (equal means the block is labels plus separators), how
// many links it has, and whether it carries a code sample (documentation,
// whatever its links).
type rawSection struct {
	heading       string
	text          string
	inlineLetters int
	linkLetters   int
	anchors       int
	hasPre        bool
}

// chromeSectionMaxRunes bounds the chrome heuristic: a short block whose links
// have no words between them is site navigation, not documentation. Longer
// link-heavy sections (an index page, a link list with prose around it) are
// kept — the cap is a deliberate compromise, since a long rail is usually a
// table of contents.
const chromeSectionMaxRunes = 400

// buildSections applies the chrome filter and the limits to the raw chunks.
// The counters were taken while walking, so a <pre> block is excluded from the
// link arithmetic, and a section carrying one is kept outright.
func buildSections(raw []rawSection, filterChrome bool, truncated bool) ([]model.Section, bool) {
	var sections []model.Section
	total := 0
	full := false // the section cap is reached
	for _, r := range raw {
		if r.text == "" {
			continue // a heading alone carries no quotable text
		}
		sec := model.Section{Order: len(sections), Heading: r.heading}
		if sec.Heading == "" {
			sec.Heading = "Top"
		}
		runes := utf8.RuneCountInString(r.text)
		if filterChrome && runes <= chromeSectionMaxRunes && !r.hasPre {
			// A short block whose text outside links contains no letter at all is
			// a rail — a breadcrumb above the title included, which is why a
			// headingless block is not exempt (an all-link page is saved by the
			// caller's unfiltered second pass). The rule is deliberately strict:
			// separators are punctuation, so one word outside a link (a caption,
			// a description after a flag, " to continue") makes the block prose.
			// A labelled menu therefore survives — the store is for prose, and
			// cutting a paragraph to remove a menu is the worse mistake.
			menu := r.inlineLetters > 0 && r.inlineLetters == r.linkLetters && r.anchors >= 2
			if menu {
				continue
			}
		}
		if full {
			// Past the cap: a block that would have been stored is a real loss.
			// Blocks this loop drops (a rail) are not, so the flag is set here
			// rather than from a raw-versus-stored count.
			truncated = true
			continue
		}
		if runes > MaxSectionChars {
			r.text = capRunes(r.text, MaxSectionChars)
			runes = utf8.RuneCountInString(r.text)
			truncated = true
		}
		if total+runes > MaxDocChars {
			r.text = capRunes(r.text, MaxDocChars-total)
			truncated = true
			if r.text != "" {
				sec.Text = r.text
				sections = append(sections, sec)
			}
			break
		}
		total += runes
		sec.Text = r.text
		sections = append(sections, sec)
		if len(sections) >= MaxSections {
			full = true
		}
	}
	return sections, truncated
}

// lettersIn counts letter runes: the chrome heuristic asks whether a word sits
// between two links, and a separator (">", "/", "|", ", ") has no letters.
func lettersIn(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			n++
		}
	}
	return n
}

// visibleRunes counts runes that are not whitespace.
func visibleRunes(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

// maxImages caps image references per doc: a page with hundreds of tracking
// pixels does not get a hundred entries in the index.
const maxImages = 64

// ExtractPlainText turns a text/plain body into sections of paragraph blocks,
// under the same limits as HTML extraction.
func ExtractPlainText(body string) (sections []model.Section, truncated bool) {
	paras := strings.FieldsFunc(body, func(r rune) bool { return r == '\n' || r == '\r' })
	var chunks []string
	var cur strings.Builder
	for _, p := range paras {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if cur.Len() > 0 && cur.Len()+len(p)+1 > MaxSectionChars {
			chunks = append(chunks, cur.String())
			cur.Reset()
		}
		if cur.Len() > 0 {
			cur.WriteByte('\n')
		}
		cur.WriteString(p)
	}
	if cur.Len() > 0 {
		chunks = append(chunks, cur.String())
	}
	total := 0
	for _, c := range chunks {
		if total+utf8.RuneCountInString(c) > MaxDocChars {
			c = capRunes(c, MaxDocChars-total)
			truncated = true
			if c == "" {
				break
			}
		}
		if utf8.RuneCountInString(c) > MaxSectionChars {
			c = capRunes(c, MaxSectionChars)
			truncated = true
		}
		total += utf8.RuneCountInString(c)
		sections = append(sections, model.Section{Order: len(sections), Heading: "Part", Text: c})
		if len(sections) >= MaxSections {
			truncated = truncated || len(chunks) > MaxSections
			break
		}
	}
	return sections, truncated
}

// voidElements have no children in HTML: a self-closing slash on any other
// element is noise the browser ignores, and so does the tree parser. svg and
// math are foreign content, where the slash does close the element.
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "link": true, "meta": true,
	"param": true, "source": true, "track": true, "wbr": true,
}

// opaqueElements are skipped whole by the title scan: their content is not page
// markup (script/style text, raw-text elements, an SVG icon's own <title>, an
// inert template).
var opaqueElements = map[string]bool{
	"script": true, "style": true, "svg": true, "math": true, "template": true,
	"noscript": true, "textarea": true, "xmp": true, "iframe": true,
	"noembed": true, "noframes": true, "plaintext": true,
}

// headElements may appear inside <head>; any other start tag closes it (HTML5
// lets </head> be omitted, and the fallback has no tree to tell it so).
var headElements = map[string]bool{
	"base": true, "basefont": true, "bgsound": true, "link": true, "meta": true,
	"title": true, "noscript": true, "noframes": true, "style": true,
	"script": true, "template": true,
}

// extractTextOnly is the fallback for markup the tree parser refuses: one
// section of page text, blocks on their own lines, <pre> kept verbatim.
// Heading structure is lost — that is the price of not building a tree the
// parser would not build — but the words are not, and an empty document would
// hide the loss.
func extractTextOnly(body string, keepChrome bool) ([]model.Section, []model.ImageRef, bool) {
	z := xhtml.NewTokenizer(strings.NewReader(body))
	var out strings.Builder
	var preBuf strings.Builder
	var images []model.ImageRef
	droppedImg := false
	blank := false
	preDepth := 0
	pendingSpace := false
	type stackEntry struct {
		name    string
		counted bool // this element incremented skipping
	}
	var stack []stackEntry
	open := map[string]int{} // open counts: an unmatched close tag costs nothing
	skipping := 0
	emitLine := func() {
		if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteByte('\n')
		}
		blank = false
		pendingSpace = false
	}
	emit := func(s string) {
		if s == "" {
			return
		}
		if blank {
			emitLine()
		} else if pendingSpace && out.Len() > 0 {
			out.WriteByte(' ')
		}
		pendingSpace = false
		out.WriteString(s)
	}
	flushPre := func() {
		if preBuf.Len() == 0 {
			return
		}
		s := normalizePre(preBuf.String())
		preBuf.Reset()
		if s == "" {
			return
		}
		if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteByte('\n')
		}
		out.WriteString(s)
		out.WriteByte('\n')
		blank = false
		pendingSpace = false
	}
	for {
		switch tt := z.Next(); tt {
		case xhtml.ErrorToken:
			flushPre()
			secs, tr := buildSections([]rawSection{{text: strings.Trim(out.String(), " \n")}}, false, droppedImg)
			return secs, images, tr
		case xhtml.TextToken:
			if skipping > 0 {
				continue
			}
			raw := string(z.Text())
			if preDepth > 0 {
				// A code sample is one run of text: inline markup inside it must not
				// split it, so the buffer is flushed on </pre>, not per node.
				preBuf.WriteString(raw)
				continue
			}
			s := inlineText(raw)
			if strings.TrimSpace(s) == "" {
				pendingSpace = true
				continue
			}
			if strings.HasPrefix(s, " ") {
				pendingSpace = true
			}
			emit(collapseText(s))
			if strings.HasSuffix(s, " ") {
				pendingSpace = true
			}
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			name, _ := z.TagName()
			n := lowerASCII(string(name))
			// A self-closing slash is only meaningful for void elements and
			// foreign content; elsewhere the browser ignores it.
			selfClosing := tt == xhtml.SelfClosingTagToken && (voidElements[n] || n == "svg" || n == "math")
			if n == "img" && skipping == 0 && preDepth == 0 {
				if ref, ok := imageFromToken(z); ok {
					if len(images) < maxImages {
						images = append(images, ref)
					} else {
						droppedImg = true
					}
				}
			}
			if n == "br" {
				// Inside a code block a break is a newline in the code; elsewhere it
				// ends the line.
				if preDepth > 0 {
					preBuf.WriteByte('\n')
				} else {
					blank = out.Len() > 0
				}
			}
			if selfClosing {
				if blockElements[n] {
					blank = out.Len() > 0
				}
				continue
			}
			if open["head"] > 0 && n != "head" && !headElements[n] {
				// </head> is optional in HTML5: the first element that cannot live
				// in the head closes it (body included).
				for i := len(stack) - 1; i >= 0; i-- {
					if stack[i].name != "head" {
						continue
					}
					for j := i; j < len(stack); j++ {
						open[stack[j].name]--
						if open[stack[j].name] <= 0 {
							delete(open, stack[j].name)
						}
						if stack[j].counted {
							skipping--
						}
					}
					stack = stack[:i]
					break
				}
			}
			// Inside a <pre> everything is text, chrome included; script and style
			// are still dropped there (the tree does the same).
			counted := dropElements[n] || (preDepth == 0 && chromeElements[n] && !keepChrome)
			stack = append(stack, stackEntry{name: n, counted: counted})
			open[n]++
			if counted {
				skipping++
			}
			if n == "pre" {
				preDepth++
			}
			if blockElements[n] {
				blank = out.Len() > 0
			}
		case xhtml.EndTagToken:
			name, _ := z.TagName()
			n := lowerASCII(string(name))
			if open[n] == 0 {
				// An unmatched close tag is not a boundary: the text it would
				// have ended never started.
				continue
			}
			if blockElements[n] {
				blank = out.Len() > 0
			}
			for i := len(stack) - 1; i >= 0; i-- {
				if stack[i].name != n {
					continue
				}
				for j := i; j < len(stack); j++ {
					e := stack[j]
					open[e.name]--
					if open[e.name] <= 0 {
						delete(open, e.name)
					}
					if e.counted {
						skipping--
					}
					if e.name == "pre" && preDepth > 0 {
						preDepth--
						flushPre()
					}
				}
				stack = stack[:i]
				break
			}
		}
	}
}

// imageFromToken reads the current img token's attributes.
func imageFromToken(z *xhtml.Tokenizer) (model.ImageRef, bool) {
	var src, alt, title string
	for {
		key, val, more := z.TagAttr()
		switch strings.ToLower(string(key)) {
		case "src":
			src = string(val)
		case "data-src":
			if src == "" {
				src = string(val)
			}
		case "alt":
			alt = string(val)
		case "title":
			title = string(val)
		}
		if !more {
			break
		}
	}
	src = strings.TrimSpace(src)
	if src == "" || strings.HasPrefix(strings.ToLower(src), "data:") {
		return model.ImageRef{}, false
	}
	return model.ImageRef{URL: src, Alt: collapseText(alt), Title: collapseText(title)}, true
}

// contentRoots returns the page's content containers in order of preference:
// <main>, then role="main", then a lone <article>. Only the outermost candidate
// is recorded — an inner element that closes first must not cut the article it
// sits in — and nested <article> elements (comment threads) count as one
// article, not as a listing.
func contentRoots(page *xhtml.Node) []*xhtml.Node {
	const (
		classMain = iota
		classRole
		classArticle
		classCount
	)
	var found [classCount]*xhtml.Node
	articles := 0
	inside := 0 // nesting depth of candidates seen so far
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		cl := -1
		if n.Type == xhtml.ElementNode {
			if dropElements[n.Data] {
				return
			}
			switch {
			case n.Data == "main":
				cl = classMain
			case hasMainRole(n):
				cl = classRole
			case n.Data == "article":
				cl = classArticle
			}
			if cl >= 0 {
				if inside == 0 {
					if found[cl] == nil {
						found[cl] = n
					}
					if cl == classArticle {
						articles++
					}
				}
				inside++
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if cl >= 0 {
			inside--
		}
	}
	walk(page)
	var roots []*xhtml.Node
	for cl := classMain; cl < classCount; cl++ {
		if found[cl] == nil {
			continue
		}
		if cl == classArticle && articles > 1 {
			continue // several articles mean a listing page
		}
		roots = append(roots, found[cl])
	}
	return roots
}

// wrapsBlock reports whether the element contains a block-level element. A link
// that wraps a paragraph or a card is formatting, not a menu entry: the HTML5
// parser also reconstructs a broken <a href> around the blocks that follow it,
// and counting those as menu labels made the section read as a rail.
func wrapsBlock(n *xhtml.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != xhtml.ElementNode {
			continue
		}
		if blockElements[c.Data] || wrapsBlock(c) {
			return true
		}
	}
	return false
}

// hasMainRole reports whether the element carries the ARIA main landmark role:
// the attribute is parsed by the tokenizer, and role is a token list
// ("main navigation" is still the main landmark).
func hasMainRole(n *xhtml.Node) bool {
	for _, a := range n.Attr {
		if a.Key != "role" {
			continue
		}
		for _, tok := range strings.Fields(a.Val) {
			if strings.EqualFold(tok, "main") {
				return true
			}
		}
	}
	return false
}

// chromeDominates reports whether the page is nothing but chrome. That is broken
// markup (a page whose every word sits in nav/aside/footer), and dropping it
// would store an empty document. The test is deliberately strict — every visible
// rune has to be chrome — because a real page keeps its chrome a minority share,
// and an unclosed <nav> swallowing the tail is accepted as the parser's (and
// the browser's) reading rather than guessed around: guessing puts real pages'
// sidebars into the document.
func chromeDominates(page *xhtml.Node) bool {
	total, chrome := textCounts(page)
	return total > 0 && chrome == total
}

// textCounts returns the visible runes of the page and of its chrome subtrees.
func textCounts(page *xhtml.Node) (total, chrome int) {
	var walk func(*xhtml.Node, bool)
	walk = func(n *xhtml.Node, inChrome bool) {
		switch n.Type {
		case xhtml.TextNode:
			r := visibleRunes(n.Data)
			total += r
			if inChrome {
				chrome += r
			}
			return
		case xhtml.ElementNode:
			if dropElements[n.Data] {
				return
			}
			inChrome = inChrome || chromeElements[n.Data]
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inChrome)
		}
	}
	walk(page, false)
	return total, chrome
}

// findNode returns the first element for which match is true, in document
// order, skipping subtrees that extraction drops anyway.
func findNode(n *xhtml.Node, match func(*xhtml.Node) bool) *xhtml.Node {
	if n.Type == xhtml.ElementNode && dropElements[n.Data] {
		return nil
	}
	if n.Type == xhtml.ElementNode && match(n) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findNode(c, match); found != nil {
			return found
		}
	}
	return nil
}

// attr returns the value of the element's attribute, or "".
func attr(n *xhtml.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// nodeText is the collapsed text of a subtree, with dropped elements skipped.
func nodeText(n *xhtml.Node) string {
	var b strings.Builder
	var walk func(*xhtml.Node)
	walk = func(m *xhtml.Node) {
		switch m.Type {
		case xhtml.TextNode:
			b.WriteString(m.Data)
			return
		case xhtml.ElementNode:
			if dropElements[m.Data] {
				return
			}
			if m.Data == "br" {
				b.WriteByte(' ')
			}
		}
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return collapseText(b.String())
}

// preText returns the raw text of a <pre> subtree: newlines and indentation are
// the content there, so nothing is collapsed; control bytes (except newline and
// tab) are stripped, trailing spaces and the surrounding blank lines are not
// stored.
func preText(n *xhtml.Node) string {
	var b strings.Builder
	var walk func(*xhtml.Node)
	walk = func(m *xhtml.Node) {
		switch m.Type {
		case xhtml.TextNode:
			b.WriteString(m.Data)
			return
		case xhtml.ElementNode:
			if dropElements[m.Data] {
				return
			}
			if m.Data == "br" {
				b.WriteByte('\n')
			}
		}
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return normalizePre(b.String())
}

// normalizePre is the verbatim normalisation shared by the tree walk and the
// tokenizer fallback: CRLF to LF, control bytes (except newline and tab) are
// stripped, trailing spaces and the surrounding blank lines are not stored.
func normalizePre(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var out strings.Builder
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, " \t")
		line = strings.Map(func(r rune) rune {
			if r == '\t' {
				return r
			}
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
				return -1
			}
			return r
		}, line)
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return strings.Trim(out.String(), "\n")
}

// junkLine drops short menu debris ("FAQ", "Log", arrows). The filter counts
// RUNES: a byte limit silently ate CJK content (one ideograph is three bytes,
// two fit under a 4-byte cut). ASCII keeps the byte-par threshold; non-ASCII
// lines survive from two runes up — CJK text legitimately packs meaning into
// two-character lines. Code blocks bypass this filter entirely.
func junkLine(line string) bool {
	switch n := utf8.RuneCountInString(line); {
	case n < 2:
		return true
	case n < 4 && isASCIIText(line):
		return true
	}
	return false
}

func isASCIIText(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// inlineText turns one text node into a piece of inline text: control bytes are
// dropped and newlines become spaces. Whitespace-only nodes keep a single space
// — inline elements split a sentence into many nodes, and dropping the space
// between two elements glues the words together.
func inlineText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	if strings.TrimSpace(b.String()) == "" {
		return " "
	}
	return b.String()
}

// collapseText strips control characters and collapses whitespace to single
// spaces. Entities are already decoded by the tokenizer, so no unescaping
// happens here — unescaping twice would turn a literal "&lt;" into "<".
func collapseText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// cleanText unescapes entities and collapses whitespace. Control characters are
// stripped AFTER unescaping: html.UnescapeString turns &#27; into ESC, so a
// scraped page can inject ANSI escapes into stored titles and from there into
// every agent terminal that prints them.
func cleanText(s string) string {
	return collapseText(html.UnescapeString(s))
}

func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// findTagStartFrom returns the index of "<name" whose name is terminated by a
// delimiter, -1 when absent. name may start with "/" for a closing tag.
func findTagStartFrom(lower, name string, from int) int {
	needle := "<" + name
	for from < len(lower) {
		i := strings.Index(lower[from:], needle)
		if i < 0 {
			return -1
		}
		i += from
		next := i + len(needle)
		if next >= len(lower) {
			return i
		}
		switch lower[next] {
		case ' ', '\t', '\n', '\r', '>', '/':
			return i
		}
		from = i + len(needle)
	}
	return -1
}
