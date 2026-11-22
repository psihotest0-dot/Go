package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// searchAnswer is an answer in the form the paid search gives (seen 2026-11-22): n results with
// long summaries and the fields the model does not use.
func searchAnswer(n int) string {
	results := make([]map[string]interface{}, n)
	for i := range results {
		results[i] = map[string]interface{}{
			"title":        fmt.Sprintf("Резултат %d ", i) + strings.Repeat("дълго заглавие ", 30),
			"site_name":    "Wikisource",
			"site_icon":    "https://cdn.example.com/site-icons/wikisource.org",
			"authority":    3,
			"rerank_score": 2.0 - float64(i)*0.1,
			"date":         "2026-11-11T16:00:00+00:00",
			"url":          fmt.Sprintf("https://example.org/page/%d", i),
			"summary":      "# Заглавие\n\n" + strings.Repeat("Изречение от страницата с текст. ", 120),
			"snippet":      "кратък откъс",
			"content":      "",
		}
	}
	raw, _ := json.Marshal(map[string]interface{}{"results": results})
	return string(raw)
}

func TestTheWebSearchGivesTheModelAShortList(t *testing.T) {
	e := newToolEnv(t)
	answer := searchAnswer(10)
	e.web.handler = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(answer)) }
	ctx, _ := e.turn("s", false)

	res, err := e.reg.ExecuteTool(ctx, "web_search", map[string]interface{}{"query": "Марк Аврелий"})
	require.NoError(t, err)
	raw, err := json.Marshal(res)
	require.NoError(t, err)
	assert.Less(t, len(raw), len(answer)/5, "the model gets a fraction of the answer (%d of %d bytes)", len(raw), len(answer))

	inner := res.(map[string]interface{})["result"].(map[string]interface{})
	list := inner["results"].([]interface{})
	require.Len(t, list, maxWebSearchResults)
	assert.Equal(t, 10-maxWebSearchResults, inner["more_results_not_shown"])
	first := list[0].(map[string]interface{})
	assert.Equal(t, "https://example.org/page/0", first["url"], "the order of the search is kept")
	assert.Equal(t, "Wikisource", first["site"])
	assert.Equal(t, "2026-11-11", first["date"])
	assert.LessOrEqual(t, utf8.RuneCountInString(first["title"].(string)), maxWebSearchTitleRunes)
	text := first["text"].(string)
	assert.LessOrEqual(t, utf8.RuneCountInString(text), maxWebSearchTextRunes)
	assert.True(t, strings.HasPrefix(text, "Заглавие Изречение"), "the summary, without the markdown heading: %q", text[:40])
	assert.True(t, strings.HasSuffix(text, "…"), "the cut is marked")
	for _, gone := range []string{"site_icon", "authority", "rerank_score", "content", "snippet", "summary"} {
		assert.NotContains(t, first, gone)
	}
	assert.Equal(t, "web", res.(map[string]interface{})["source"], "the notice that the text is untrusted stays")
}

func TestASearchAnswerOfAnotherFormIsLeftAsItCame(t *testing.T) {
	odd := map[string]interface{}{"answer": "текст", "items": []interface{}{1}}
	assert.Equal(t, odd, trimSearchResult(odd))

	few := trimSearchResult(map[string]interface{}{"results": []interface{}{
		map[string]interface{}{"title": "А", "url": "https://a.org", "snippet": "откъс", "summary": ""},
		"not an object",
	}})
	assert.Equal(t, []interface{}{map[string]interface{}{"title": "А", "url": "https://a.org", "text": "откъс"}}, few["results"],
		"the snippet when there is no summary; a result that is not an object is left out")
	assert.NotContains(t, few, "more_results_not_shown")
}

func TestCutAtWord(t *testing.T) {
	assert.Equal(t, "кратко", cutAtWord("кратко", 10))
	assert.Equal(t, "едно две…", cutAtWord("едно две три четири", 12))
	assert.Equal(t, "абвгдежзий…", cutAtWord("абвгдежзийклмн", 11), "no space: cut inside the word")
}
