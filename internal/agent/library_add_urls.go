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
func userURLKeys(messages []openai.ChatCompletionMessage) map[string]bool {
	keys := map[string]bool{}
	for _, m := range messages {
		if m.Role != openai.ChatMessageRoleUser {
			continue
		}
		for _, u := range extractURLs(m.Content) {
			if k := urlKey(u); k != "" {
				keys[k] = true
			}
		}
	}
	return keys
}
