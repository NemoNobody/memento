package client

import (
	"html"
	"strings"
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
var dropTags = []string{"script", "style", "noscript", "template", "svg", "select", "option", "head"}

// blockTags end a text line in the extraction, approximating block layout.
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
func ExtractSections(body string) (sections []model.Section, images []model.ImageRef, truncated bool) {
	lower := lowerASCII(body)
	body = stripElements(lower, body)
	lower = lowerASCII(body)

	// Pass 1: linear scan. Tag names drive structure; text accumulates into
	// the current raw section.
	var raw []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			raw = append(raw, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(body); {
		lt := strings.IndexByte(body[i:], '<')
		if lt < 0 {
			cur.WriteString(body[i:])
			break
		}
		cur.WriteString(body[i : i+lt])
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
			cur.WriteByte('<')
			i = tagStart + 1
			continue
		}
		switch {
		case !isClose && name == "img":
			if ref, ok := imageRef(body[tagStart:tagEnd]); ok {
				if len(images) < maxImages {
					images = append(images, ref)
				}
			}
		case !isClose && headingTags[name]:
			flush()
		case blockTags[name]:
			cur.WriteByte('\n')
		}
		i = tagEnd
	}
	flush()

	// Pass 2: normalise. The first non-empty line of a raw section is its
	// heading when short (that is what a heading tag yields); the rest is body.
	total := 0
	for _, r := range raw {
		lines := contentLines(r)
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
		if utf8.RuneCountInString(text) > MaxSectionChars {
			text = capRunes(text, MaxSectionChars)
			truncated = true
		}
		if total+utf8.RuneCountInString(text) > MaxDocChars {
			text = capRunes(text, MaxDocChars-total)
			truncated = true
			if text != "" {
				sec.Text = text
				sections = append(sections, sec)
			}
			break
		}
		total += utf8.RuneCountInString(text)
		sec.Text = text
		sections = append(sections, sec)
		if len(sections) >= MaxSections {
			truncated = truncated || len(raw) > len(sections)
			break
		}
	}
	return sections, images, truncated
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
	// Skip "<img".
	rest := tag
	if i := strings.IndexByte(rest, ' '); i >= 0 {
		rest = rest[i+1:]
	}
	if i := strings.LastIndexByte(rest, '>'); i >= 0 {
		rest = rest[:i]
	}
	rest = strings.TrimSuffix(strings.TrimSpace(rest), "/")
	for i := 0; i < len(rest); {
		// key
		j := i
		for j < len(rest) && rest[j] != '=' && rest[j] != ' ' && rest[j] != '\t' && rest[j] != '\n' {
			j++
		}
		key := strings.ToLower(rest[i:j])
		if key == "" {
			i = j + 1
			continue
		}
		for j < len(rest) && (rest[j] == ' ' || rest[j] == '\t' || rest[j] == '\n') {
			j++
		}
		if j >= len(rest) || rest[j] != '=' {
			attrs[key] = ""
			i = j + 1
			continue
		}
		j++ // past '='
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
		if len(line) < 4 {
			continue
		}
		lines = append(lines, line)
	}
	return lines
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
