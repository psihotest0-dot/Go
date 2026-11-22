package agent

import (
	"strings"
	"unicode/utf8"
)

// The answer of the paid web search is cut to what the model uses before it reaches the model
// (plan step 0.4, W1, 2026-11-22). A real answer is 15-37 KB: ten results, each with a summary of
// up to 4 000 characters, a snippet, an icon address, an "authority" and an empty "content"; on
// 2026-11-21 one search filled the budget of the round and was cut at its end. Now: the first
// maxWebSearchResults results in the order the search ranked them, each with its title, address,
// site, date and one text of at most maxWebSearchTextRunes.
//
// Why no limit on rerank_score: measured on four real searches (measurements\websearch_2026-11-22)
// its scale moves with the query - 0.57-0.70 for a technical one, 1.5-2.4 for news, 1.07-1.56 for
// a question in Bulgarian. The limit of 1.5 that the plan named would have kept 1 result of 10 in
// the first search, and none in the technical one.

const (
	// maxWebSearchResults is how many results reach the model.
	maxWebSearchResults = 6
	// maxWebSearchTitleRunes and maxWebSearchTextRunes bound the title and the text of a result.
	maxWebSearchTitleRunes = 100
	maxWebSearchTextRunes  = 500
	// webSearchDateLen keeps the day of a date ("2026-11-11T16:00:00+00:00" -> "2026-11-11").
	webSearchDateLen = len("2006-01-02")
)

// trimSearchResult is the answer of the search as the model gets it. An answer without a list of
// results is left as it came: the search may change its form, and the model then still sees it.
func trimSearchResult(answer map[string]interface{}) map[string]interface{} {
	list, ok := answer["results"].([]interface{})
	if !ok {
		return answer
	}
	out := make(map[string]interface{}, len(answer)+1)
	for k, v := range answer {
		out[k] = v
	}
	kept := make([]interface{}, 0, min(len(list), maxWebSearchResults))
	left := 0
	for i, item := range list {
		if len(kept) == maxWebSearchResults {
			left = len(list) - i
			break
		}
		result, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		kept = append(kept, trimSearchItem(result))
	}
	out["results"] = kept
	if left > 0 {
		out["more_results_not_shown"] = left
	}
	return out
}

// trimSearchItem is one result with only the fields the model uses.
func trimSearchItem(result map[string]interface{}) map[string]interface{} {
	text := func(name string) string {
		s, _ := result[name].(string)
		return strings.Join(strings.Fields(s), " ")
	}
	item := map[string]interface{}{}
	if title := text("title"); title != "" {
		item["title"] = cutAtWord(title, maxWebSearchTitleRunes)
	}
	if link := text("url"); link != "" {
		item["url"] = link
	}
	if site := text("site_name"); site != "" {
		item["site"] = site
	}
	if date := text("date"); date != "" {
		if len(date) > webSearchDateLen {
			date = date[:webSearchDateLen]
		}
		item["date"] = date
	}
	for _, name := range []string{"summary", "snippet", "content"} {
		if body := strings.TrimLeft(text(name), "# "); body != "" {
			item["text"] = cutAtWord(body, maxWebSearchTextRunes)
			break
		}
	}
	return item
}

// cutAtWord shortens s to at most limit runes, at the last space when there is one in the second
// half, and marks the cut with "…".
func cutAtWord(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)[:limit-1]
	if i := strings.LastIndexByte(string(runes), ' '); i > len(string(runes))/2 {
		return strings.TrimRight(string(runes)[:i], " ,.;:-") + "…"
	}
	return string(runes) + "…"
}
