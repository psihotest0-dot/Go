package answerplan

import (
	"fmt"
	"strings"
)

// The note of a planned reply goes next to the person's message in the copy for the model,
// as the closing note of П49 does, and replaces it (it says how the reply ends too). It is
// never stored. The memories are given by number; the model marks a sentence that uses one
// with [n], and the code takes the marks out of the stream and counts them (step 2.3).
const (
	notePrefix = "\n\n[Note for the answer, not from the user: the person is telling you something personal. " +
		"Reply in 3-6 sentences, warmly and simply, in this order:\n" +
		"1. show in a few words that you understood what they say;\n" +
		"2. weave in what you know about them from these memories, as things you remember, not as a list:\n"
	noteRules = "Use only these memories: say nothing else about their life, their habits or the people close to them. " +
		"Put the number of a memory, like [1], right after the sentence that uses it.\n" +
		"3. add one short thought of your own about it - your own words: not a quote, no book, no author;\n"
	noteEndQuestion  = "4. end with ONE short question about a specific detail of their words - not a generic one, not two.]"
	noteEndStatement = "4. end with a statement (a thought, an observation or an offer), not with a question.]"
	// maxMemoryRunes keeps a long memory from filling the note.
	maxMemoryRunes = 300
)

// Note is the note for the model.
func (p *Plan) Note() string {
	var b strings.Builder
	b.WriteString(notePrefix)
	for i, m := range p.Memories {
		fmt.Fprintf(&b, "[%d] %s\n", i+1, cut(strings.Join(strings.Fields(m.Content), " "), maxMemoryRunes))
	}
	b.WriteString(noteRules)
	if p.AskQuestion {
		b.WriteString(noteEndQuestion)
	} else {
		b.WriteString(noteEndStatement)
	}
	return b.String()
}

// WithoutNote is content without the note of a plan, if it has one; whatever another note put
// after it stays.
func WithoutNote(content string) string {
	start := strings.LastIndex(content, notePrefix)
	if start < 0 {
		return content
	}
	rest := content[start:]
	for _, end := range []string{noteEndQuestion, noteEndStatement} {
		if i := strings.Index(rest, end); i >= 0 {
			return content[:start] + rest[i+len(end):]
		}
	}
	return content
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
