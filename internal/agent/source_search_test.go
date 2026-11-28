package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/taskrun"
)

func TestAsksToFindSources(t *testing.T) {
	for _, m := range []string{
		"опитай от раздел 11 да потърсиш в интернет дали някой от източнижите може да се извлече", // the Flet chat of 2026-11-21, word for word
		"намери в интернет източниците от списъка и ги свали",
		"find the sources of these references on the internet",
	} {
		assert.True(t, asksToFindSources(m), m)
	}
	for _, m := range []string{
		"мислех че се подразбираше да започнеш..,започни търсенето на първият източник", // one source
		"намери в интернет статии за OCR и направи справочник във файл",                 // a research (web_research.go)
		"какви източници има за стоицизма в книгите ми",                                 // nothing to look for
	} {
		assert.False(t, asksToFindSources(m), m)
	}
}

// Lines of a list of references as the owner pasted one on 2026-11-20 (public papers).
const referenceLines = `Lou, Polk, Skouras, [A Tug of War: Overnight Versus Intraday Expected Returns](https://researchonline.lse.ac.uk/id/eprint/87481) (JFE 2019).
- Ang, Hodrick, Xing, Zhang; Bali, Cakici, Whitelaw, [Maxing out](https://econpapers.repec.org/RePEc:eee:jfinec:v:99:y:2011:i:2:p:427-446) (JFE 2011).
- Bajgrowicz, Scaillet, [Technical trading revisited](https://a.example/Bajgrowicz_Scaillet.pdf) (JFE 2012); Sullivan, Timmermann, White (1999).
- Daniel, Moskowitz, [Momentum Crashes](https://www.nber.org/papers/w20439); George, Hwang (2004) за 52-седмичния връх.`

func TestParseReferences(t *testing.T) {
	refs := parseReferences("Ето ги:\n" + referenceLines + "\nКажи ми, ако нещо липсва.")
	var names []string
	for _, r := range refs {
		names = append(names, r.Title+"|"+r.Year+"|"+r.Link)
	}
	assert.Equal(t, []string{
		"A Tug of War: Overnight Versus Intraday Expected Returns|2019|https://researchonline.lse.ac.uk/id/eprint/87481",
		"||",
		"Maxing out|2011|https://econpapers.repec.org/RePEc:eee:jfinec:v:99:y:2011:i:2:p:427-446",
		"Technical trading revisited|2012|https://a.example/Bajgrowicz_Scaillet.pdf",
		"|1999|",
		"Momentum Crashes||https://www.nber.org/papers/w20439",
		"|2004|",
	}, names)
	assert.Equal(t, "Ang, Hodrick, Xing, Zhang", refs[1].Text)
	assert.Equal(t, "Sullivan Timmermann White 1999 pdf", referenceQuery(refs[4]))
	assert.Equal(t, "George Hwang 2004 pdf", referenceQuery(refs[6]))
	assert.Equal(t, "Maxing out Bali pdf", referenceQuery(refs[2]))
	assert.Equal(t, "Ang Hodrick Xing Zhang pdf", referenceQuery(refs[1]))
	assert.Equal(t, "Momentum Crashes (Daniel)", referenceName(refs[5]))
	assert.Empty(t, parseReferences("Здравей, как си днес? Аз съм добре."), "prose is no list")
}

func TestFullTextLink(t *testing.T) {
	assert.Equal(t, "https://a.example/p.pdf", fullTextLink("https://a.example/p.pdf"))
	assert.Equal(t, "https://arxiv.org/pdf/2512.23847", fullTextLink("https://arxiv.org/abs/2512.23847"))
	assert.Equal(t, "https://b.example/download/pdf/123", fullTextLink("https://b.example/download/pdf/123"))
	assert.Equal(t, "https://c.example/thesis.pdf", fullTextLink("https://c.example/thesis.pdf#31#45#31"), "the mark of a passage is no part of the page")
	for _, l := range []string{
		"https://econpapers.repec.org/paper/x.pdf",  // a page of an abstract
		"https://papers.ssrn.com/sol3/Delivery.pdf", // refuses programs
		"https://www.nber.org/papers/w20439",        // the page of a paper, not its text
		"http://localhost/p.pdf",                    // not public
	} {
		assert.Empty(t, fullTextLink(l), l)
	}
}

func TestPickFullTextTakesAPDFOfTheSameWork(t *testing.T) {
	ref := reference{Title: "Momentum Crashes", Text: "Daniel, Moskowitz, Momentum Crashes", Link: "https://www.nber.org/papers/w20439"}
	hits := []webHit{
		{Title: "Momentum Crashes | NBER", Link: "https://www.nber.org/papers/w20439"},
		{Title: "Value and momentum everywhere", Link: "https://c.example/value.pdf"},
		{Title: "Momentum crashes - Kent Daniel", Link: "https://d.example/papers/momentum-crashes.pdf"},
	}
	assert.Equal(t, "https://d.example/papers/momentum-crashes.pdf", pickFullText(hits, ref))
	// The search of 2026-11-28: passages from inside works that cite the reference are not it.
	byName := reference{Text: "George, Hwang (2004) за 52-седмичния връх", Year: "2004"}
	assert.Empty(t, pickFullText([]webHit{
		{Title: "George and Hwang (2004) also suggest that since the returns on the 52-week strategy", Link: "https://g.example/FulltextThesis.pdf#31#45#31"},
	}, byName), "without a title nothing can be told from a thesis that cites it")
	vol := reference{Title: "On the Performance of Volatility-Managed Portfolios", Text: "Cederburg, O'Doherty, Wang, Yan"}
	assert.Equal(t, "https://h.example/docs/b37.pdf", pickFullText([]webHit{
		{Title: "Panel B reports the performance in-sample and net of transaction costs of volatility-managed portfolios", Link: "https://i.example/DeMiguel.pdf#2#9#2"},
		{Title: "On the performance of volatility-managed portfolios", Link: "https://h.example/docs/b37.pdf#1#7#1"},
	}, vol))
	mclean := reference{Title: "Does Academic Research Destroy Stock Return Predictability?"}
	assert.Empty(t, pickFullText([]webHit{
		{Title: "Do Cross-Sectional Predictors Contain Systematic Information?", Link: "https://h.example/docs/ec3.pdf",
			Text: "McLean and Pontiff (2016) show that academic research destroys stock return predictability"},
	}, mclean), "another paper, though its text names the reference")
	assert.True(t, matchesReference(webHit{Title: "Maxing out: Stocks as lotteries and the cross-section of expected returns"}, reference{Title: "Maxing out"}),
		"a list may cut the title")
	assert.False(t, matchesReference(webHit{Title: "Momentum everywhere"}, reference{Title: "Momentum"}), "one word is no title to begin with")
}

func TestSectionOf(t *testing.T) {
	doc := "# Ревю\n\n## 10. Изводи\nтекст\n## 11. Източници\n- A, [X](https://a.example/x) (2019).\n- B, C (2004).\n## 12. Бележки\nдруго\n"
	part, ok := sectionOf(doc, "11")
	require.True(t, ok)
	assert.Equal(t, "- A, [X](https://a.example/x) (2019).\n- B, C (2004).\n", part)

	plain := "10. Изводи\nтекст\n11-източници\n1. A, [X](https://a.example/x) (2019).\n2. B, C (2004).\n12. Бележки\nдруго"
	part, ok = sectionOf(plain, "11")
	require.True(t, ok)
	assert.Equal(t, "1. A, [X](https://a.example/x) (2019).\n2. B, C (2004).\n", part)

	_, ok = sectionOf(doc, "7")
	assert.False(t, ok)
}

// sourcesEnv is the env of the tasks over links with a search of its own: query -> results.
func sourcesEnv(t *testing.T, results map[string]string) (*taskRunEnv, *fakeFetcher, *[]string) {
	t.Helper()
	e, f, _ := linkTaskEnv(t)
	var asked []string
	var mu sync.Mutex
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Query string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		asked = append(asked, body.Query)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		for k, v := range results {
			if strings.Contains(body.Query, k) {
				_, _ = w.Write([]byte(v))
				return
			}
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	t.Cleanup(web.Close)
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	e.reg.webBaseURL, e.reg.httpClient = web.URL, web.Client()
	return e, f, &asked
}

func TestTheSourcesOfAListAreFoundAndProposed(t *testing.T) {
	e, f, asked := sourcesEnv(t, map[string]string{
		"Maxing out Ang": `{"results":[{"title":"Maxing out: Stocks as lotteries and the cross-section of expected returns","url":"https://e.example/maxing.pdf#1#30#1"}]}`,
	})
	e.say(t, "s", "ето раздел 11:\n- Lou, Polk, [A Tug of War](https://a.example/paper.pdf) (JFE 2019).\n"+
		"- Ang, Hodrick, [Maxing out](https://c.example/abstract) (JFE 2011).\n- Sullivan, Timmermann, White (1999).")
	asksBefore := len(e.model.Requests)

	out := streamedText(t, e.say(t, "s", "опитай от раздел 11 да потърсиш в интернет дали някой от източниците може да се извлече"))

	assert.Equal(t, asksBefore, len(e.model.Requests), "the code answers")
	assert.Contains(t, out, "Справките в по-ранното ти съобщение са 3")
	assert.Contains(t, out, "С твоя линк, който става (1):\n• A Tug of War (Lou, 2019) — PDF")
	assert.Contains(t, out, "Намерих пълния текст другаде (1)")
	assert.Contains(t, out, "• Maxing out (Ang, 2011) (линкът ти: страницата има само резюме")
	assert.Contains(t, out, "\n  → https://e.example/maxing.pdf\n")
	assert.Contains(t, out, "Не намерих (1):\n• Sullivan, Timmermann, White (1999) — няма линк; без заглавие не търся")
	assert.Contains(t, out, "чак след твоето „да“")
	assert.Contains(t, out, "Да започна ли?")
	assert.Equal(t, []string{"Maxing out Ang pdf"}, *asked, "searched only for the one with a title and no working link")

	p, err := e.plans.Proposed(context.Background(), e.userID, "s")
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, taskrun.KindKeyFacts, p.Kind)
	require.Len(t, p.Steps, 2)
	assert.Equal(t, "https://a.example/paper.pdf", p.Steps[0].Link)
	assert.Equal(t, "https://e.example/maxing.pdf", p.Steps[1].Link)
	assert.Equal(t, "Maxing out", p.Steps[1].Expect, "the runner checks that the document is this work")
	assert.Empty(t, p.Steps[0].Expect, "the person's own link is theirs")
	assert.NotContains(t, f.asked, "https://e.example/maxing.pdf", "a link of the search is not opened before the yes")

	start := streamedText(t, e.say(t, "s", "да"))
	assert.Contains(t, start, "Започнах")
}

func TestTheSourcesOfASectionOfTheAttachedFile(t *testing.T) {
	e, _, _ := sourcesEnv(t, nil)
	e.attach(t, e.userID, "s", "ревю.md", "## 10. Изводи\nтекст\n## 11. Източници\n- A, [X](https://a.example/paper.pdf) (2019).\n- B, C (2004).\n## 12. Друго\n- D, E (2001).\n")

	out := streamedText(t, e.say(t, "s", "опитай от раздел 11 да потърсиш в интернет дали някой от източниците може да се извлече"))

	assert.Contains(t, out, "Справките в раздел 11 на „ревю.md“ са 2")
	assert.NotContains(t, out, "D, E")
}

func TestAskingForSourcesWithoutAListIsLeftToTheModel(t *testing.T) {
	e, _, asked := sourcesEnv(t, nil)
	before := len(e.model.Requests)
	e.say(t, "s", "намери в интернет източниците за стоицизма")
	assert.Empty(t, *asked)
	assert.Greater(t, len(e.model.Requests), before, "the model answers")
}
