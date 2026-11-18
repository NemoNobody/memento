package client

import (
	"html"
	"strings"
	"unicode"
	"unicode/utf8"

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

// dropTags are removed together with everything inside them: they carry
// scripts and styling, not readable text. head is dropped: its title is
// taken separately (ExtractTitle), the rest (meta, link, style) is noise.
// nav/aside/footer are page chrome — a link rail is not documentation; a page
// whose main content is not marked up keeps its text through the chrome
// filter's fallback (see buildSections).
var dropTags = []string{"script", "style", "noscript", "template", "svg", "select", "option", "head", "nav", "aside", "footer"}

// blockTags end a text line in the extraction, approximating block layout.
// The chrome elements are listed too: dropTags removes a well-formed one with
// its content, but an unclosed or self-closed <nav> only loses its tag, and its
// text must still end a line instead of merging into the previous paragraph.
var blockTags = map[string]bool{
	"p": true, "br": true, "div": true, "li": true, "ul": true, "ol": true,
	"table": true, "tr": true, "td": true, "th": true, "pre": true,
	"blockquote": true, "section": true, "article": true, "header": true,
	"footer": true, "nav": true, "aside": true, "h1": true, "h2": true,
	"h3": true, "h4": true, "h5": true, "h6": true, "dd": true, "dt": true,
	"form": true, "figure": true, "figcaption": true, "hr": true,
}

// headingTags open a new section. Deep headings (h5/h6) are treated as plain
// text: a section tree is not the goal, a handful of quotable chunks is.
var headingTags = map[string]bool{"h1": true, "h2": true, "h3": true, "h4": true}

// lowerASCII lowercases ASCII A-Z bytes and leaves every other byte untouched,
// so the result is always byte-length-equal to the input. strings.ToLower is
// unsafe for the body/lower index sharing in this file: Go's Unicode lowering
// changes byte length (U+2126 OHM SIGN shrinks 3 to 2 bytes, U+023A grows 2 to
// 3), which desynchronizes the shared byte indices and panics on slice bounds
// with ordinary prose (an "Ω" in a technical article is enough).
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
// page has no title.
func ExtractTitle(body string) string {
	lower := lowerASCII(body)
	open := findTagStart(lower, "title")
	if open < 0 {
		return ""
	}
	openEnd := strings.IndexByte(lower[open:], '>')
	if openEnd < 0 {
		return ""
	}
	textStart := open + openEnd + 1
	close := findTagStart(lower, "/title")
	if close < 0 || close < textStart {
		return ""
	}
	title := cleanText(body[textStart:close])
	return capRunes(title, MaxTitleChars)
}

// ExtractSections splits page HTML into heading-delimited text sections and
// collects referenced images (url+alt+title; binaries are never stored). It
// reports whether the limits truncated the content.
//
// The extraction prefers the page's main-content container when the markup
// names one (<main>, role="main", <article>): sidebar links and footers are
// not documentation, and a rail that comes first in the source would eat the
// document budget before the article starts.
func ExtractSections(body string) (sections []model.Section, images []model.ImageRef, truncated bool) {
	clean := stripElements(lowerASCII(body), body)
	lower := lowerASCII(clean)
	for _, s := range contentSpans(lower, clean) {
		if secs, imgs, tr := extractFrom(lower[s.start:s.end], clean[s.start:s.end]); len(secs) > 0 {
			return secs, imgs, tr
		}
	}
	return extractFrom(lower, clean)
}

// contentSpans returns the byte spans of the page's content containers, in
// order of preference: <main>, then role="main", then a lone <article>.
//
// The scan is one pass over a stack of open elements. The obvious shape — try
// every candidate with a nesting-aware scan for its close tag — is quadratic on
// hostile input, and fetch_max_bytes allows 2 MB: a 240 KB page of repeated
// unclosed <main> cost 35 seconds of CPU, and a deep stack of <div> followed by
// a run of unrelated close tags cost 15 more (both measured). Hence the two
// constant-time guards: a name that is not open cannot be closed, and "a
// candidate is open below me" is computed once at push.
//
// Only the outermost candidate is taken: an inner element that closes first
// must not cut the article it sits in. An element that never closes is not
// offered at all — falling back to the whole page is the safe direction, since
// the fallback keeps content and only loses the chrome filtering.
func contentSpans(lower, body string) []span {
	const (
		classMain = iota
		classRole
		classArticle
		classCount
	)
	type openElement struct {
		name  string
		pos   int
		class int
		// candsBelow/artsBelow are the counters at push time: recomputing a
		// counter after popping is then a lookup of the entry below instead of
		// a scan, and accept() answers "is an outer candidate still open?" in
		// O(1) instead of walking the stack per candidate.
		candsBelow int
		artsBelow  int
	}
	var found [classCount]span
	var have [classCount]bool
	var stack []openElement
	var open map[string]int // open counts per tag name: unmatched close tags are free
	cands, arts, articles := 0, 0, 0

	accept := func(idx, end int) {
		cl := stack[idx].class
		// An outer candidate that is still open is the content root; this one
		// sits inside it.
		if cl < 0 || have[cl] || stack[idx].candsBelow > 0 {
			return
		}
		found[cl] = span{stack[idx].pos, end}
		have[cl] = true
	}

	popTo := func(k int) {
		stack = stack[:k]
		cands, arts = 0, 0
		if k == 0 {
			return
		}
		e := stack[k-1]
		cands = e.candsBelow
		if e.class >= 0 {
			cands++
		}
		arts = e.artsBelow
		if e.name == "article" {
			arts++
		}
	}

	for i := 0; i < len(lower); {
		lt := strings.IndexByte(lower[i:], '<')
		if lt < 0 {
			break
		}
		p := i + lt
		if strings.HasPrefix(lower[p:], "<!--") {
			// A commented-out "</main>" is not the end of the article.
			j := strings.Index(lower[p+4:], "-->")
			if j < 0 {
				break
			}
			i = p + 4 + j + 3
			continue
		}
		name, tagEnd, isClose := parseTag(body, lower, p)
		if tagEnd <= p {
			i = p + 1
			continue
		}
		i = tagEnd
		if name == "" {
			continue
		}
		if isClose {
			if open[name] == 0 {
				continue
			}
			for k := len(stack) - 1; k >= 0; k-- {
				if stack[k].name != name {
					continue
				}
				accept(k, tagEnd)
				for j := k; j < len(stack); j++ {
					open[stack[j].name]--
				}
				popTo(k)
				break
			}
			continue
		}
		if isSelfClosing(body[p:tagEnd]) {
			continue
		}
		cl := -1
		switch {
		case name == "main":
			cl = classMain
		case roleHasMain(lower[p:tagEnd]):
			cl = classRole
		case name == "article":
			cl = classArticle
			// Only a top-level <article> counts: one nested for comments is
			// still a single-article page.
			if arts == 0 {
				articles++
			}
		}
		if open == nil {
			open = map[string]int{}
		}
		stack = append(stack, openElement{name: name, pos: p, class: cl, candsBelow: cands, artsBelow: arts})
		open[name]++
		if cl >= 0 {
			cands++
		}
		if name == "article" {
			arts++
		}
	}

	// Several <article> elements mean a listing page, not an article: one
	// teaser is not the document.
	var spans []span
	for cl := classMain; cl <= classArticle; cl++ {
		if !have[cl] || (cl == classArticle && articles > 1) {
			continue
		}
		spans = append(spans, found[cl])
	}
	return spans
}

// span is a byte range in a page.
type span struct{ start, end int }

// roleHasMain reports whether the tag carries the ARIA main landmark role. The
// attribute is parsed, not substring-matched: "data-role=main" is a different
// attribute and "role=mainnav" a different role, and a quoted token list
// ("main navigation") is still the main landmark. tag must already be
// lowercased.
func roleHasMain(tag string) bool {
	if !strings.Contains(tag, "role") {
		return false // most open tags: skip building an attribute map
	}
	for _, tok := range strings.Fields(htmlAttrs(tag)["role"]) {
		if strings.EqualFold(tok, "main") {
			return true
		}
	}
	return false
}

func isSelfClosing(tag string) bool {
	return strings.HasSuffix(strings.TrimSpace(tag), "/>")
}

// rawSection is one heading-delimited chunk of text plus the number of its
// visible (non-whitespace) runes that sat inside links — the signal for a
// navigation block. Whitespace is excluded on both sides of the ratio: block
// tags inject newlines that no author typed, and counting them would dilute a
// rail of links down to innocent-looking prose.
type rawSection struct {
	text      string
	linkRunes int
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

// extractFrom is the actual splitter: it runs over one HTML span (the content
// root when the page has one), already stripped of dropTags by the caller. The
// two arguments are the same bytes, the second one ASCII-lowercased; that is
// what lets the scan search a lowercased copy while slicing the original.
func extractFrom(lower, body string) (sections []model.Section, images []model.ImageRef, truncated bool) {

	// Pass 1: linear scan. Tag names drive structure; text accumulates into
	// the current raw section.
	var raw []rawSection
	var cur strings.Builder
	linkRunes := 0
	inLink := false
	flush := func() {
		if cur.Len() > 0 {
			raw = append(raw, rawSection{text: cur.String(), linkRunes: linkRunes})
			cur.Reset()
			linkRunes = 0
		}
		// An unclosed <a> must not poison the next section: one broken anchor
		// would otherwise make every later block look like a rail.
		inLink = false
	}
	writeText := func(s string) {
		if s == "" {
			return
		}
		cur.WriteString(s)
		if inLink {
			linkRunes += visibleRunes(s)
		}
	}
	for i := 0; i < len(body); {
		lt := strings.IndexByte(body[i:], '<')
		if lt < 0 {
			writeText(body[i:])
			break
		}
		writeText(body[i : i+lt])
		tagStart := i + lt
		if strings.HasPrefix(lower[tagStart:], "<!--") {
			// Comments are author notes, not readable content.
			if j := strings.Index(lower[tagStart+4:], "-->"); j >= 0 {
				i = tagStart + 4 + j + 3
			} else {
				i = len(body)
			}
			continue
		}
		if strings.HasPrefix(lower[tagStart:], "<!") || strings.HasPrefix(lower[tagStart:], "<?") {
			// Declarations (doctype, CDATA, XML processing instructions) are
			// never text: a doctype line once became a document's heading.
			if j := strings.IndexByte(lower[tagStart:], '>'); j >= 0 {
				i = tagStart + j + 1
			} else {
				i = len(body)
			}
			continue
		}
		name, tagEnd, isClose := parseTag(body, lower, tagStart)
		if name == "" {
			// Not a real tag ("<" in prose); keep it as text.
			writeText("<")
			i = tagStart + 1
			continue
		}
		switch {
		case !isClose && name == "img":
			if ref, ok := imageRef(body[tagStart:tagEnd]); ok {
				if len(images) < maxImages {
					images = append(images, ref)
				} else {
					truncated = true // a usable img beyond the cap: the drop must be visible
				}
			}
		case !isClose && name == "a":
			// <a id="anchor"/> is an anchor target, not an opener: taking it as
			// one would count the rest of the block as link text.
			inLink = !isSelfClosing(body[tagStart:tagEnd])
		case isClose && name == "a":
			inLink = false
		case !isClose && headingTags[name]:
			flush()
		case blockTags[name]:
			cur.WriteByte('\n')
		}
		i = tagEnd
	}
	flush()

	sections, truncated = buildSections(raw, true, truncated)
	if len(sections) == 0 && len(raw) > 0 {
		// A page that is nothing but link blocks is still a page: dropping
		// every section would store an empty document and hide the loss.
		sections, truncated = buildSections(raw, false, truncated)
	}
	return sections, images, truncated
}

// chromeSectionMaxRunes bounds the chrome heuristic: a short block that is
// mostly links is site navigation, not documentation. Longer link-heavy
// sections (an index page, a link list with prose around it) are kept.
const chromeSectionMaxRunes = 400

// buildSections is pass 2: it normalises raw chunks into sections, applies the
// limits and (optionally) drops short link-dominated blocks.
func buildSections(raw []rawSection, filterChrome bool, truncated bool) ([]model.Section, bool) {
	var sections []model.Section
	total := 0
	full := false // the section cap is reached
	for _, r := range raw {
		lines := contentLines(r.text)
		if len(lines) == 0 {
			continue
		}
		sec := model.Section{Order: len(sections)}
		if utf8.RuneCountInString(lines[0]) <= maxHeadingChars {
			sec.Heading = lines[0]
			lines = lines[1:]
		}
		if sec.Heading == "" {
			sec.Heading = "Top"
		}
		if len(lines) == 0 {
			continue // a heading alone carries no quotable text
		}
		text := strings.Join(lines, "\n")
		runes := utf8.RuneCountInString(text)
		if filterChrome && sec.Heading != "Top" && runes <= chromeSectionMaxRunes {
			// A short block that is mostly anchor text is a link rail.
			if all := visibleRunes(r.text); all > 0 && r.linkRunes*5 >= all*3 {
				continue
			}
		}
		if full {
			// Past the cap: a block that would have been stored is a real loss.
			// Blocks this loop drops (junk, a rail) are not, so the flag is set
			// here rather than from a raw-versus-stored count.
			truncated = true
			continue
		}
		if runes > MaxSectionChars {
			text = capRunes(text, MaxSectionChars)
			runes = utf8.RuneCountInString(text)
			truncated = true
		}
		if total+runes > MaxDocChars {
			text = capRunes(text, MaxDocChars-total)
			truncated = true
			if text != "" {
				sec.Text = text
				sections = append(sections, sec)
			}
			break
		}
		total += runes
		sec.Text = text
		sections = append(sections, sec)
		if len(sections) >= MaxSections {
			full = true
		}
	}
	return sections, truncated
}

// maxImages caps image references per doc: a page with hundreds of tracking
// pixels does not get a hundred entries in the index.
const maxImages = 64

// imageRef parses an <img ...> tag into {url, alt, title}. An img without a
// usable src is skipped (tracking beacons with empty src are noise).
func imageRef(tag string) (model.ImageRef, bool) {
	attrs := htmlAttrs(tag)
	src := attrs["src"]
	if src == "" {
		src = attrs["data-src"]
	}
	if src == "" || strings.HasPrefix(strings.ToLower(src), "data:") {
		return model.ImageRef{}, false
	}
	return model.ImageRef{
		URL:   strings.TrimSpace(src),
		Alt:   cleanText(attrs["alt"]),
		Title: cleanText(attrs["title"]),
	}, true
}

// htmlAttrs parses a tag's attributes (double/single/unquoted values) into a
// lowercased-key map. It only reads the tag, never the document.
func htmlAttrs(tag string) map[string]string {
	attrs := map[string]string{}
	// Skip the tag name: any whitespace ends it, not just a space (a newline or
	// a tab after "<div" is valid markup).
	rest := tag
	if i := strings.IndexAny(rest, " \t\n\r"); i >= 0 {
		rest = rest[i+1:]
	}
	if i := strings.LastIndexByte(rest, '>'); i >= 0 {
		rest = rest[:i]
	}
	rest = strings.TrimSuffix(strings.TrimSpace(rest), "/")
	for i := 0; i < len(rest); {
		// key
		j := i
		for j < len(rest) && rest[j] != '=' && rest[j] != ' ' && rest[j] != '\t' && rest[j] != '\n' && rest[j] != '\r' {
			j++
		}
		key := strings.ToLower(rest[i:j])
		if key == "" {
			i = j + 1
			continue
		}
		k := j
		for k < len(rest) && (rest[k] == ' ' || rest[k] == '\t' || rest[k] == '\n' || rest[k] == '\r') {
			k++
		}
		if k >= len(rest) || rest[k] != '=' {
			// A boolean attribute: the next key starts at the delimiter, not
			// after it ("hidden role=main" must not become "ole=main").
			attrs[key] = ""
			i = j
			continue
		}
		j = k + 1 // past '='
		for j < len(rest) && (rest[j] == ' ' || rest[j] == '\t' || rest[j] == '\n' || rest[j] == '\r') {
			j++ // role = "main" is valid HTML
		}
		var val string
		switch {
		case j < len(rest) && (rest[j] == '"' || rest[j] == '\''):
			q := rest[j]
			end := strings.IndexByte(rest[j+1:], q)
			if end < 0 {
				val = rest[j+1:]
				i = len(rest)
			} else {
				val = rest[j+1 : j+1+end]
				i = j + 2 + end
			}
		default:
			end := j
			for end < len(rest) && rest[end] != ' ' && rest[end] != '\t' && rest[end] != '\n' {
				end++
			}
			val = rest[j:end]
			i = end + 1
		}
		if _, dup := attrs[key]; !dup {
			attrs[key] = val
		}
	}
	return attrs
}

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

// stripElements removes dropTags (and everything inside them) from body. It
// runs as one left-to-right pass: the old per-occurrence rebuild
// (body[:open]+body[end:] for every tag) was quadratic — a 2 MB page made of
// style tags burned minutes of CPU and GC in a long-lived server, driven
// there by a single untrusted link (the project's threat model).
// lower is the ASCII-lowercased body (byte-length equal, so indices stay in
// sync).
func stripElements(lower, body string) string {
	var b strings.Builder
	b.Grow(len(body))
	// noCloseFrom[tag] = offset from which a scan proved that no closing tag
	// exists in the rest of the body. Without it, every unclosed drop-tag
	// re-scans the whole tail: a page of repeated unclosed <style> turns the
	// single pass quadratic (the exact hostile-input pattern of this package).
	noCloseFrom := map[string]int{}
	for i := 0; i < len(lower); {
		lt := strings.IndexByte(lower[i:], '<')
		if lt < 0 {
			b.WriteString(body[i:])
			break
		}
		pos := i + lt
		b.WriteString(body[i:pos])
		tag := matchDropTag(lower, pos)
		if tag == "" {
			b.WriteByte(body[pos])
			i = pos + 1
			continue
		}
		openEnd := strings.IndexByte(lower[pos:], '>')
		if openEnd < 0 {
			// Unterminated open tag: keep the rest verbatim. Markup is broken
			// from here; deleting anything on a guess would silently lose text.
			b.WriteString(body[pos:])
			break
		}
		contentStart := pos + openEnd + 1
		close := -1
		if proven, seen := noCloseFrom[tag]; !seen || proven > contentStart {
			close = findTagStartFrom(lower, "/"+tag, contentStart)
			if close < 0 {
				noCloseFrom[tag] = contentStart
			}
		}
		if close < 0 {
			// Self-closed or unclosed element: drop only the tag itself.
			// Deleting the whole tail would silently lose real content —
			// unclosed <style>/<svg> and self-closed <svg/> are common.
			i = contentStart
			continue
		}
		closeEnd := strings.IndexByte(lower[close:], '>')
		if closeEnd < 0 {
			b.WriteString(body[pos:])
			break
		}
		i = close + closeEnd + 1 // drop open..close inclusive
	}
	return b.String()
}

// matchDropTag returns the dropTag that starts at pos — "<name" terminated
// by a tag delimiter — or "" when the tag at pos is kept.
func matchDropTag(lower string, pos int) string {
	rest := lower[pos:]
	for _, tag := range dropTags {
		needle := "<" + tag
		if !strings.HasPrefix(rest, needle) {
			continue
		}
		next := pos + len(needle)
		if next >= len(lower) {
			return tag // name ends the body: it is the whole tag
		}
		switch lower[next] {
		case ' ', '\t', '\n', '\r', '>', '/':
			return tag
		}
	}
	return ""
}

// findTagStart returns the index of "<name" whose name is terminated by a
// delimiter, -1 when absent. name may start with "/" for a closing tag.
func findTagStart(lower, name string) int {
	return findTagStartFrom(lower, name, 0)
}

// findTagStartFrom is findTagStart searching from a byte offset.
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

// parseTag reads the tag at pos: its name, end index (after '>'), and whether
// it is a closing tag. An empty name means the "<" is not a tag.
func parseTag(body, lower string, pos int) (name string, end int, isClose bool) {
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
		return "", pos + 1, false
	}
	tagEnd := strings.IndexByte(lower[k:], '>')
	if tagEnd < 0 {
		// No '>' until EOF: the whole tail is markup. Report the name so the
		// caller jumps past the tail in one step; an empty name here would
		// take the "< in prose" path (advance one byte) and re-scan the tail
		// at every following '<' — quadratic on a "<a<a<a" soup (measured).
		return lower[start:k], len(body), isClose
	}
	return lower[start:k], k + tagEnd + 1, isClose
}

func isAlpha(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }
func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// contentLines turns one raw text block into normalised lines: entities
// unescaped, whitespace collapsed, junk (short menu debris) dropped.
func contentLines(raw string) []string {
	var lines []string
	for _, line := range strings.Split(raw, "\n") {
		line = cleanText(line)
		// The junk filter counts RUNES: a byte limit silently drops CJK
		// content (one ideograph is three bytes, two fit under a 4-byte
		// cut). ASCII keeps the old byte-par threshold ("FAQ", "Log",
		// arrows); non-ASCII lines survive from two runes up — CJK text
		// legitimately packs meaning into two-character lines.
		switch n := utf8.RuneCountInString(line); {
		case n < 2:
			continue
		case n < 4 && isASCIIText(line):
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

func isASCIIText(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// cleanText unescapes entities and collapses whitespace to single spaces.
// Control characters are stripped AFTER unescaping: html.UnescapeString turns
// &#27; into ESC, so a scraped page can inject ANSI escapes into stored
// titles/sections and from there into every agent terminal that prints them.
func cleanText(s string) string {
	s = html.UnescapeString(s)
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

func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
