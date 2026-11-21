package telegram

import (
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wellFormed checks the HTML Telegram is sent: only the tags this file makes, each closed in the
// order it was opened, no stray "<" or ">" in the text, and "&" only as an entity.
func wellFormed(h string) error {
	tag := regexp.MustCompile(`^<(/?)(b|i|s|code|pre|a)( href="[^"<>]*")?>`)
	var stack []string
	for i := 0; i < len(h); {
		switch h[i] {
		case '<':
			m := tag.FindStringSubmatch(h[i:])
			if m == nil {
				return fmt.Errorf("unknown or broken tag at %d: %q", i, h[i:min(i+30, len(h))])
			}
			if m[1] == "" {
				stack = append(stack, m[2])
			} else {
				if len(stack) == 0 || stack[len(stack)-1] != m[2] {
					return fmt.Errorf("closing </%s> does not match the open tags %v", m[2], stack)
				}
				stack = stack[:len(stack)-1]
			}
			i += len(m[0])
		case '>':
			return fmt.Errorf("stray > at %d", i)
		case '&':
			if !regexp.MustCompile(`^&(amp|lt|gt|quot|#34|#39);`).MatchString(h[i:]) {
				return fmt.Errorf("bare & at %d: %q", i, h[i:min(i+12, len(h))])
			}
			i++
		default:
			i++
		}
	}
	if len(stack) != 0 {
		return fmt.Errorf("tags left open: %v", stack)
	}
	return nil
}

func TestMarkdownToHTML(t *testing.T) {
	tests := []struct{ name, in, want string }{
		// emphasis
		{"bold with stars", "**bold**", "<b>bold</b>"},
		{"bold with underscores", "__bold__", "<b>bold</b>"},
		{"italic with a star", "*it*", "<i>it</i>"},
		{"italic with an underscore", "_it_", "<i>it</i>"},
		{"strike", "~~gone~~", "<s>gone</s>"},
		{"italic inside bold", "**bold *and italic* text**", "<b>bold <i>and italic</i> text</b>"},
		{"bold inside italic", "*it **and bold** text*", "<i>it <b>and bold</b> text</i>"},
		{"bold inside bold is flattened", "**a __b__ c**", "<b>a b c</b>"},
		{"Cyrillic", "**Здравей**, _свят_!", "<b>Здравей</b>, <i>свят</i>!"},
		{"around punctuation", "(**bold**) and «_it_»", "(<b>bold</b>) and «<i>it</i>»"},

		// what is not emphasis
		{"snake case", "use snake_case_name here", "use snake_case_name here"},
		{"Cyrillic inside a word", "дума_друга_трета", "дума_друга_трета"},
		{"multiplication", "2 * 3 * 4 = 24", "2 * 3 * 4 = 24"},
		{"stars inside a word", "a*b*c", "a*b*c"},
		{"power", "5 ** 2", "5 ** 2"},
		{"no partner", "**not closed", "**not closed"},
		{"a single star", "a * b", "a * b"},
		{"a lone underscore", "_", "_"},
		{"mark followed by a space", "** bold **", "** bold **"},
		{"escaped marks", `\*not italic\* and \_this\_`, "*not italic* and _this_"},
		{"closing mark before a letter", "**bold**text", "**bold**text"},
		// Each rule alone: the pairs above are also stopped by another rule, these are not.
		{"an opening mark inside a word has a partner", "foo_bar_ baz", "foo_bar_ baz"},
		{"a star inside a word has a partner", "a*b* c", "a*b* c"},
		{"an opening mark followed by a space has a partner", "x * a* y", "x * a* y"},
		{"a closing mark after a space is no partner", "*a * b", "*a * b"},
		{"a closing mark before a letter is no partner", "*a*b c", "*a*b c"},
		{"an escaped mark inside emphasis is not its end", `*a \* b*`, "<i>a * b</i>"},
		{"a longer run of quotes does not close a code span", "`a``b`", "`a`<code>b</code>"},

		// escaping
		{"markup in the answer", "a < b && c > d", "a &lt; b &amp;&amp; c &gt; d"},
		{"html written by the model", "<script>alert(1)</script> <b>x</b>", "&lt;script&gt;alert(1)&lt;/script&gt; &lt;b&gt;x&lt;/b&gt;"},
		{"an entity written by the model", "&amp; &lt;", "&amp;amp; &amp;lt;"},

		// code
		{"inline code", "run `go test` now", "run <code>go test</code> now"},
		{"markup in inline code", "`a <b> & **c**`", "<code>a &lt;b&gt; &amp; **c**</code>"},
		{"double quotes hold a quote", "``a`b``", "<code>a`b</code>"},
		{"an unclosed quote", "a ` b", "a ` b"},
		{"emphasis does not reach into code", "**a `b**` c**", "<b>a <code>b**</code> c</b>"},
		{"fenced block", "```go\nfmt.Println(\"<a>\")\n```", "<pre>fmt.Println(\"&lt;a&gt;\")</pre>"},
		{"markup in a fenced block", "```\n**not bold**\n- not a bullet\n```", "<pre>**not bold**\n- not a bullet</pre>"},
		{"an unclosed fence", "text\n```\ncode", "text\n<pre>code</pre>"},
		{"an empty fence", "```\n```", ""},
		{"text around a fence", "before\n```\nx\n```\nafter **b**", "before\n<pre>x</pre>\nafter <b>b</b>"},

		// links
		{"a link", "[Go](https://go.dev/doc)", `<a href="https://go.dev/doc">Go</a>`},
		{"a link with parentheses", "[Go](https://en.wikipedia.org/wiki/Go_(language))", `<a href="https://en.wikipedia.org/wiki/Go_(language)">Go</a>`},
		{"an ampersand in a link", "[x](https://a.b/?q=1&r=2)", `<a href="https://a.b/?q=1&amp;r=2">x</a>`},
		{"a quote in a link", `[x](https://a.b/"onmouseover="y)`, `<a href="https://a.b/&#34;onmouseover=&#34;y">x</a>`},
		{"emphasis in a link text", "[**Go**](http://go.dev)", `<a href="http://go.dev"><b>Go</b></a>`},
		{"javascript is not a link", "[x](javascript:alert(1))", "[x](javascript:alert(1))"},
		{"tg is not a link", "[x](tg://resolve?domain=a)", "[x](tg://resolve?domain=a)"},
		{"a link with a space is text", "[x](https://a.b/ c)", "[x](https://a.b/ c)"},
		{"an empty label is text", "[](https://a.b)", "[](https://a.b)"},
		{"brackets that are not a link", "a [b] c (d)", "a [b] c (d)"},
		{"a link in a link text", "[a [b](https://x.y) c](https://z.w)", `<a href="https://z.w">a [b](https://x.y) c</a>`},

		// blocks
		{"heading", "## Title", "<b>Title</b>"},
		{"heading with closing hashes", "## Title ##", "<b>Title</b>"},
		{"heading that ends with a hash", "# Learn C#", "<b>Learn C#</b>"},
		{"heading with bold in it", "### **Bold** title", "<b>Bold title</b>"},
		{"a hashtag is not a heading", "#hashtag", "#hashtag"},
		{"an empty heading", "# ", ""},
		{"bullets", "- one\n* two\n  - nested\n+ plus", "• one\n• two\n  • nested\n• plus"},
		{"bold at the start of a bullet", "* **x**: y", "• <b>x</b>: y"},
		{"a bold line is not a bullet", "**x** and y", "<b>x</b> and y"},
		{"numbered list", "1. one\n2. **two**", "1. one\n2. <b>two</b>"},
		{"a table stays text", "| **a** | b |\n|---|---|\n| 1 | 2 |", "| **a** | b |\n|---|---|\n| 1 | 2 |"},
		{"a quote stays text", "> quoted & <b>", "&gt; quoted &amp; &lt;b&gt;"},
		{"lines stay lines", "one\n\ntwo\nthree", "one\n\ntwo\nthree"},
		{"windows line ends", "a\r\n**b**", "a\n<b>b</b>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := markdownToHTML(tc.in)
			assert.Equal(t, tc.want, got)
			assert.NoError(t, wellFormed(got), "the HTML must be well formed: %q", got)
		})
	}
}

// Whatever the model writes, the HTML is one Telegram can take: balanced tags and no bare
// markup characters. A fixed seed makes a failure reproducible.
func TestMarkdownToHTMLIsAlwaysWellFormed(t *testing.T) {
	alphabet := []string{"*", "**", "_", "__", "~~", "`", "``", "```\n", "[", "]", "(", ")", "https://a.b", "\\", "<", ">", "&", "#", "# ", "- ", "| ", "\n", " ", "a", "b", "Я", "&amp;", "<b>", "http://x.y/?a=1&b=2"}
	r := rand.New(rand.NewSource(20261121))
	for n := 0; n < 30000; n++ {
		var b strings.Builder
		for k := r.Intn(24); k >= 0; k-- {
			b.WriteString(alphabet[r.Intn(len(alphabet))])
		}
		in := b.String()
		out := markdownToHTML(in)
		if err := wellFormed(out); err != nil {
			t.Fatalf("input %q gave %q: %v", in, out, err)
		}
	}
}

func TestSplitReplyKeepsShortTextAsOnePiece(t *testing.T) {
	assert.Equal(t, []string{"Здравей"}, splitReply("Здравей", 4000))
	assert.Equal(t, []string{strings.Repeat("я", 4000)}, splitReply(strings.Repeat("я", 4000), 4000))
}

func TestSplitReplyCutsAtTheBlankLineFirst(t *testing.T) {
	a, b := strings.Repeat("а", 3000), strings.Repeat("б", 3000)

	got := splitReply(a+"\n\n"+b, 4000)

	assert.Equal(t, []string{a, b}, got)
}

func TestSplitReplyCutsAtALineEndThenAtASpace(t *testing.T) {
	line := strings.Repeat("x", 100)
	text := strings.Repeat(line+"\n", 60) // 6060 characters, no blank line

	got := splitReply(text, 4000)

	require.Len(t, got, 2)
	assert.True(t, strings.HasSuffix(got[0], line), "the first piece ends at a line end: ...%q", got[0][len(got[0])-5:])

	words := strings.TrimSpace(strings.Repeat("слово ", 1500)) // no newline at all
	pieces := splitReply(words, 4000)
	require.Len(t, pieces, 3)
	for _, p := range pieces {
		assert.False(t, strings.HasPrefix(p, "ово") || strings.HasSuffix(p, "сл"), "no word is cut in two: %q", p[:10])
	}
}

func TestSplitReplyNeverMakesAPieceLongerThanTheLimit(t *testing.T) {
	texts := map[string]string{
		"one word":        strings.Repeat("я", 9000),
		"Cyrillic prose":  strings.Repeat("Това е изречение. ", 700),
		"a code block":    "```go\n" + strings.Repeat("fmt.Println(\"x\")\n", 600) + "```\nend",
		"two code blocks": "```\n" + strings.Repeat("a\n", 1900) + "```\ntext\n```\n" + strings.Repeat("b\n", 1900) + "```",
	}
	for name, text := range texts {
		t.Run(name, func(t *testing.T) {
			for _, p := range splitReply(text, 4000) {
				assert.LessOrEqual(t, utf8.RuneCountInString(p), 4000)
				assert.NotEmpty(t, strings.TrimSpace(p))
			}
		})
	}
}

// A cut inside a code block closes the block and opens it again: every piece is a whole
// document, and the code of the pieces is the code of the answer.
func TestSplitReplyClosesAndReopensACodeBlockItCutsThrough(t *testing.T) {
	var code []string
	for i := 0; i < 700; i++ {
		code = append(code, fmt.Sprintf("line %d <x> & y", i))
	}
	text := "intro\n```go\n" + strings.Join(code, "\n") + "\n```\nthe end **bold**"

	pieces := splitReply(text, 4000)

	require.Greater(t, len(pieces), 2)
	var got []string
	for _, p := range pieces {
		assert.False(t, insideFence(p), "every piece closes the block it opens: %q", p[max(0, len(p)-20):])
		html := markdownToHTML(p)
		require.NoError(t, wellFormed(html))
		for _, l := range strings.Split(p, "\n") {
			if strings.HasPrefix(l, "line ") {
				got = append(got, l)
			}
		}
	}
	assert.Equal(t, code, got, "no line of the code is lost or repeated")
	assert.Contains(t, markdownToHTML(pieces[len(pieces)-1]), "<b>bold</b>")
}

func TestInsideFence(t *testing.T) {
	assert.False(t, insideFence("text"))
	assert.True(t, insideFence("```\ncode"))
	assert.False(t, insideFence("```\ncode\n```"))
	assert.True(t, insideFence("a\n  ```go\nb"))
	assert.False(t, insideFence("`inline` code ```not at the start"))
}
