package agent

import (
	"net/url"
	"regexp"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

// urlPattern finds an address in the text a person wrote: it runs to the first space or
// to a character that cannot be in a link written in a message.
var urlPattern = regexp.MustCompile("https?://[^\\s<>\"'`\\]]+")

// extractURLs returns the addresses in text, as written, without the punctuation that
// follows them in a sentence or a Markdown link.
func extractURLs(text string) []string {
	var out []string
	for _, found := range urlPattern.FindAllString(text, -1) {
		if u := trimURLEnd(found); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// trimURLEnd takes off what is at the end of a link in prose: a full stop, a comma, a
// bracket of a Markdown link ("[x](https://a.org/b)") - but not the bracket that belongs to
// the address ("https://doi.org/10.1016/S0304-405X(98)00026-9").
func trimURLEnd(u string) string {
	for {
		trimmed := strings.TrimRight(u, ".,;:!?*_")
		if strings.HasSuffix(trimmed, ")") && strings.Count(trimmed, "(") < strings.Count(trimmed, ")") {
			trimmed = trimmed[:len(trimmed)-1]
		}
		if trimmed == u {
			return u
		}
		u = trimmed
	}
}

// urlKey is what two spellings of one address have in common, so that the link the model
// sends back is the link the person wrote even when it dropped a slash, a fragment or the
// "s" of https. It is "" for something that is not an address.
func urlKey(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return ""
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	key := host + strings.TrimRight(u.EscapedPath(), "/")
	if u.RawQuery != "" {
		key += "?" + u.RawQuery
	}
	return key
}

// userURLKeys lists the keys (see urlKey) of the addresses in the messages the person
// wrote. What the assistant wrote, and what tools returned, is not here: a tool that
// fetches addresses fetches only the ones the person gave, so that an address can never be
// the way out for something the model read.
func userURLKeys(messages []openai.ChatCompletionMessage) map[string]string {
	keys := map[string]string{}
	for _, m := range messages {
		if m.Role != openai.ChatMessageRoleUser {
			continue
		}
		for _, u := range extractURLs(m.Content) {
			if k := urlKey(u); k != "" {
				if _, known := keys[k]; !known {
					keys[k] = u
				}
			}
		}
	}
	return keys
}

const (
	// maxLinkTypos is how many characters a link of the model may differ from the person's.
	maxLinkTypos = 4
	// minCorrectableKeyRunes: a short address differs from another by a few characters
	// without being a copy of it ("a.org/x", "a.org/y").
	minCorrectableKeyRunes = 24
)

// nearestLink returns the link the person wrote that the key is a slightly wrong copy of:
// on the same host, at most maxLinkTypos characters away (and no more than a tenth of the
// address), and strictly nearer than every other link of the person.
func nearestLink(key string, written map[string]string) (string, bool) {
	host := hostOfKey(key)
	best, second := maxLinkTypos+1, maxLinkTypos+1
	var found string
	for other, link := range written {
		if hostOfKey(other) != host || len([]rune(other)) < minCorrectableKeyRunes {
			continue
		}
		d := editDistance(key, other)
		switch {
		case d < best:
			best, second, found = d, best, link
		case d < second:
			second = d
		}
	}
	if best > maxLinkTypos || best*10 > len([]rune(key)) || second == best {
		return "", false
	}
	return found, true
}

// hostOfKey is the host in a key made by urlKey.
func hostOfKey(key string) string {
	host, _, _ := strings.Cut(key, "/")
	host, _, _ = strings.Cut(host, "?")
	return host
}

// editDistance is the number of single-character changes (insert, delete, replace) that
// turn a into b.
func editDistance(a, b string) int {
	x, y := []rune(a), []rune(b)
	prev := make([]int, len(y)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(x); i++ {
		cur := make([]int, len(y)+1)
		cur[0] = i
		for j := 1; j <= len(y); j++ {
			cost := 1
			if x[i-1] == y[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(y)]
}
