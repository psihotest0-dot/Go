package telegram

import (
	"html"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The answers of the model are Markdown; Telegram shows a message as plain text unless it is
// told a parse mode, so the stars and underscores of the answer were printed as they are. This
// file turns the Markdown into the HTML subset Telegram understands (<b>, <i>, <s>, <code>,
// <pre>, <a href>) and splits a long answer so that no piece has a tag cut in two. The
// conversion is a small, forgiving one: whatever it does not understand (tables, nested lists,
// an emphasis mark with no partner) stays as the text it was, escaped.

var (
	// A heading may end with closing hashes after a space ("## Title ##"); "# Learn C#" keeps its #.
	headingRe = regexp.MustCompile(`^ {0,3}#{1,6}\s+(.*?)(?:\s+#+)?\s*$`)
	bulletRe  = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
)

// escapable are the characters a backslash makes literal: "\*" is a star.
const escapable = "\\`*_{}[]()#+-.!~|>"

// textEscape escapes what Telegram's HTML reads as markup.
func textEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// markdownToHTML converts one piece of an answer. Every tag it opens it closes, in order, so the
// result is always well formed; Telegram can still refuse a message (see sendAnswer), and the
// caller then sends the Markdown as it is.
func markdownToHTML(src string) string {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// A fenced block: everything up to the closing fence (or the end) is code, as it is.
		if strings.HasPrefix(trimmed, "```") {
			var code []string
			j := i + 1
			for ; j < len(lines); j++ {
				if strings.HasPrefix(strings.TrimSpace(lines[j]), "```") {
					break
				}
				code = append(code, lines[j])
			}
			if body := strings.Join(code, "\n"); strings.TrimSpace(body) != "" {
				out = append(out, "<pre>"+textEscape(body)+"</pre>")
			}
			i = j
			continue
		}

		// A table stays text: Telegram has no table and the cells are not worth the markup.
		if strings.HasPrefix(trimmed, "|") && strings.Count(trimmed, "|") >= 2 {
			out = append(out, textEscape(line))
			continue
		}

		if m := headingRe.FindStringSubmatch(line); m != nil {
			if strings.TrimSpace(m[1]) == "" {
				out = append(out, "") // nothing to make bold
				continue
			}
			out = append(out, "<b>"+renderInline(m[1], inlineState{bold: true})+"</b>")
			continue
		}

		if m := bulletRe.FindStringSubmatch(line); m != nil {
			out = append(out, m[1]+"• "+renderInline(m[2], inlineState{}))
			continue
		}

		out = append(out, renderInline(line, inlineState{}))
	}
	return strings.Join(out, "\n")
}

// inlineState says which tags are open around the text being rendered: a tag is not opened
// inside itself, and a link has no link in it.
type inlineState struct {
	bold, italic, strike, link bool
}

// renderInline renders the emphasis, code and links of one line.
func renderInline(s string, st inlineState) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]

		// \* and the like: the character itself.
		if c == '\\' && i+1 < len(s) && strings.IndexByte(escapable, s[i+1]) >= 0 {
			b.WriteString(textEscape(string(s[i+1])))
			i += 2
			continue
		}

		if c == '`' {
			if end, text, ok := codeSpan(s, i); ok {
				b.WriteString("<code>" + textEscape(text) + "</code>")
				i = end
				continue
			}
		}

		if c == '[' && !st.link {
			if end, label, url, ok := linkAt(s, i); ok {
				b.WriteString(`<a href="` + attrEscape(url) + `">` + renderInline(label, inlineState{bold: st.bold, italic: st.italic, strike: st.strike, link: true}) + "</a>")
				i = end
				continue
			}
		}

		if marker, tag, ok := emphasisAt(s, i); ok {
			if closer := findCloser(s, i+len(marker), marker); closer >= 0 {
				inner := s[i+len(marker) : closer]
				next := st
				open := false
				switch tag {
				case "b":
					open, next.bold = !st.bold, true
				case "i":
					open, next.italic = !st.italic, true
				case "s":
					open, next.strike = !st.strike, true
				}
				rendered := renderInline(inner, next)
				if open {
					b.WriteString("<" + tag + ">" + rendered + "</" + tag + ">")
				} else {
					b.WriteString(rendered)
				}
				i = closer + len(marker)
				continue
			}
		}

		// Any other character: itself. A multi-byte character is copied whole.
		_, size := utf8.DecodeRuneInString(s[i:])
		b.WriteString(textEscape(s[i : i+size]))
		i += size
	}
	return b.String()
}

// attrEscape escapes a value for an HTML attribute.
func attrEscape(s string) string {
	return html.EscapeString(s)
}

// codeSpan reads `code` (or ``code``) starting at s[i]; end is just after the closing quotes.
func codeSpan(s string, i int) (end int, text string, ok bool) {
	n := 0
	for i+n < len(s) && s[i+n] == '`' {
		n++
	}
	fence := strings.Repeat("`", n)
	closing := strings.Index(s[i+n:], fence)
	if closing <= 0 { // not closed, or empty
		return 0, "", false
	}
	inner := s[i+n : i+n+closing]
	// A longer run of quotes is not this span's closer.
	if i+n+closing+n < len(s) && s[i+n+closing+n] == '`' {
		return 0, "", false
	}
	return i + n + closing + n, inner, true
}

// linkAt reads [label](url) starting at s[i]. Only http and https links are links; any other
// scheme is left as text (a link the model wrote must not be a way to open something else).
func linkAt(s string, i int) (end int, label, url string, ok bool) {
	// The label ends at the "]" that balances the "[": a label may hold brackets of its own.
	closeLabel, depth := -1, 0
scan:
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '[':
			depth++
		case ']':
			if depth == 0 {
				closeLabel = j
				break scan
			}
			depth--
		}
	}
	if closeLabel < 0 {
		return
	}
	if closeLabel+1 >= len(s) || s[closeLabel+1] != '(' {
		return
	}
	depth = 0
	for j := closeLabel + 2; j < len(s); j++ {
		switch s[j] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				label, url = s[i+1:closeLabel], strings.TrimSpace(s[closeLabel+2:j])
				lower := strings.ToLower(url)
				if label == "" || !(strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://")) ||
					strings.ContainsFunc(url, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
					return 0, "", "", false
				}
				return j + 1, label, url, true
			}
			depth--
		}
	}
	return
}

// emphasisAt reports the emphasis mark that starts at s[i] and the tag it makes: "**" and "__"
// bold, "*" and "_" italic, "~~" strike. A mark that is not at the start of a word (an opening
// mark has no space after it, and an underscore or star inside a word is just a character:
// snake_case, 2*3*4) is not one.
func emphasisAt(s string, i int) (marker, tag string, ok bool) {
	switch {
	case strings.HasPrefix(s[i:], "**"):
		marker, tag = "**", "b"
	case strings.HasPrefix(s[i:], "__"):
		marker, tag = "__", "b"
	case strings.HasPrefix(s[i:], "~~"):
		marker, tag = "~~", "s"
	case s[i] == '*':
		marker, tag = "*", "i"
	case s[i] == '_':
		marker, tag = "_", "i"
	default:
		return "", "", false
	}
	after := i + len(marker)
	if after >= len(s) {
		return "", "", false
	}
	if r, _ := utf8.DecodeRuneInString(s[after:]); unicode.IsSpace(r) || strings.HasPrefix(s[after:], marker) {
		return "", "", false
	}
	if i > 0 {
		if r, _ := utf8.DecodeLastRuneInString(s[:i]); isWordRune(r) {
			return "", "", false
		}
	}
	return marker, tag, true
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// findCloser finds the mark that closes the emphasis opened before from: not after a space, not
// before a letter or digit, outside code, and not the half of a longer mark. -1 when there is none.
func findCloser(s string, from int, marker string) int {
	for i := from; i < len(s); {
		switch {
		case s[i] == '\\' && i+1 < len(s):
			i += 2
			continue
		case s[i] == '`':
			if end, _, ok := codeSpan(s, i); ok {
				i = end
				continue
			}
		case strings.HasPrefix(s[i:], marker):
			// A single mark inside which a double one of the same character sits ("*a **b** c*")
			// skips the double mark whole.
			if len(marker) == 1 && i+1 < len(s) && s[i+1] == marker[0] {
				i += 2
				continue
			}
			if i > from {
				prev, _ := utf8.DecodeLastRuneInString(s[:i])
				nextWord := false
				if end := i + len(marker); end < len(s) {
					next, _ := utf8.DecodeRuneInString(s[end:])
					nextWord = isWordRune(next)
				}
				if !unicode.IsSpace(prev) && !nextWord {
					return i
				}
			}
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return -1
}

// splitReply cuts an answer into pieces of at most limit characters, at a blank line if it can,
// else at a line end, else at a space. A code block that a cut falls into is closed at the end
// of its piece and opened again at the start of the next one, so every piece converts on its own.
func splitReply(text string, limit int) []string {
	const fence = "```"
	var chunks []string
	rest := []rune(text)
	prefix := ""
	for len(rest) > 0 {
		room := limit - utf8.RuneCountInString(prefix) - len(fence) - 1 // for the closing fence
		if len(rest) <= limit-utf8.RuneCountInString(prefix) {
			chunks = append(chunks, prefix+string(rest))
			break
		}
		cut := cutPoint(rest, room)
		piece := prefix + strings.TrimRight(string(rest[:cut]), " \t\n")
		prefix = ""
		if insideFence(piece) {
			piece += "\n" + fence
			prefix = fence + "\n"
		}
		chunks = append(chunks, piece)
		rest = []rune(strings.TrimLeft(string(rest[cut:]), " \t\n"))
	}
	return chunks
}

// insideFence reports whether a code block is still open at the end of text: an odd number of
// lines that begin with a fence.
func insideFence(text string) bool {
	open := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			open = !open
		}
	}
	return open
}

// cutPoint is where to cut runes[:room]: after the last blank line, else the last line end, else
// the last space, else at room.
func cutPoint(runes []rune, room int) int {
	if room > len(runes) {
		room = len(runes)
	}
	for _, sep := range []string{"\n\n", "\n", " "} {
		if i := strings.LastIndex(string(runes[:room]), sep); i > 0 {
			return utf8.RuneCountInString(string(runes[:room])[:i]) + utf8.RuneCountInString(sep)
		}
	}
	return room
}
