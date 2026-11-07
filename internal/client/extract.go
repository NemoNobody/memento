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
// scripts and styling, not readable text.
var dropTags = []string{"script", "style", "noscript", "template", "svg", "select", "option"}

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

// ExtractTitle returns the text of the <title> element, capped. Empty when the
// page has no title.
func ExtractTitle(body string) string {
	lower := strings.ToLower(body)
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
	lower := strings.ToLower(body)
	body = stripElements(lower, body)
	lower = strings.ToLower(body)

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

// stripElements removes dropTags and their content from body. lower is the
// lowercased body (byte-length equal, so indices stay in sync).
func stripElements(lower, body string) string {
	for _, tag := range dropTags {
		for {
			open := findTagStart(lower, tag)
			if open < 0 {
				break
			}
			openEnd := strings.IndexByte(lower[open:], '>')
			if openEnd < 0 {
				break
			}
			contentStart := open + openEnd + 1
			end := len(body)
			if close := findTagStart(lower, "/"+tag); close >= contentStart {
				if closeEnd := strings.IndexByte(lower[close:], '>'); closeEnd >= 0 {
					end = close + closeEnd + 1
				}
			}
			body = body[:open] + body[end:]
			lower = lower[:open] + lower[end:]
		}
	}
	return body
}

// findTagStart returns the index of "<name" whose name is terminated by a
// delimiter, -1 when absent. name may start with "/" for a closing tag.
func findTagStart(lower, name string) int {
	needle := "<" + name
	for from := 0; ; {
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
		return "", len(body), isClose
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
func cleanText(s string) string {
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
