package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	return sourcesEnvWith(t, results, nil)
}

// sourcesEnvWith is sourcesEnv with a catalogue of its own: a part of the address of a lookup -> its
// answer (none: an empty list). What was asked is in the list returned: the queries of the search,
// "openalex: <lookup>" for the catalogue.
func sourcesEnvWith(t *testing.T, results, catalogue map[string]string) (*taskRunEnv, *fakeFetcher, *[]string) {
	t.Helper()
	e, f, _ := linkTaskEnv(t)
	var asked []string
	var mu sync.Mutex
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/works") {
			lookup, _ := url.QueryUnescape(r.URL.RawQuery)
			mu.Lock()
			asked = append(asked, "openalex: "+lookup)
			mu.Unlock()
			for k, v := range catalogue {
				if strings.Contains(lookup, k) {
					_, _ = w.Write([]byte(v))
					return
				}
			}
			_, _ = w.Write([]byte(`{"results":[]}`))
			return
		}
		var body struct{ Query string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		asked = append(asked, body.Query)
		mu.Unlock()
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
	e.reg.webBaseURL, e.reg.httpClient, e.reg.openAlexURL = web.URL, web.Client(), web.URL+"/works"
	return e, f, &asked
}

// stwWork is the work of "Sullivan, Timmermann, White (1999)" as the catalogue gave it on 2026-11-28.
const stwWork = `{"results":[{"title":"Data‐Snooping, Technical Trading Rule Performance, and the Bootstrap","publication_year":1999,
	"authorships":[{"author":{"display_name":"Ryan Sullivan"}},{"author":{"display_name":"Allan Timmermann"}},{"author":{"display_name":"Halbert White"}}],
	"best_oa_location":{"pdf_url":"https://onlinelibrary.wiley.com/doi/pdfdirect/10.1111/0022-1082.00163"},
	"locations":[{"pdf_url":"https://onlinelibrary.wiley.com/doi/pdfdirect/10.1111/0022-1082.00163"},{"pdf_url":"http://eprints.example/119144/1/dp303.pdf"},{"pdf_url":"https://researchonline.example/id/eprint/119144/1/dp303.pdf"}]}]}`

func TestTheSourcesOfAListAreFoundAndProposed(t *testing.T) {
	// The catalogue knows nothing of "Maxing out" here: the search is asked for it.
	e, f, asked := sourcesEnvWith(t, map[string]string{
		"Maxing out Ang": `{"results":[{"title":"Maxing out: Stocks as lotteries and the cross-section of expected returns","url":"https://e.example/maxing.pdf#1#30#1"}]}`,
	}, map[string]string{"raw_author_name.search:Sullivan": stwWork})
	e.say(t, "s", "ето раздел 11:\n- Lou, Polk, [A Tug of War](https://a.example/paper.pdf) (JFE 2019).\n"+
		"- Ang, Hodrick, [Maxing out](https://c.example/abstract) (JFE 2011).\n- Sullivan, Timmermann, White (1999).\n- Smith (2004).")
	asksBefore := len(e.model.Requests)

	out := streamedText(t, e.say(t, "s", "опитай от раздел 11 да потърсиш в интернет дали някой от източниците може да се извлече"))

	assert.Equal(t, asksBefore, len(e.model.Requests), "the code answers")
	assert.Contains(t, out, "Справките в по-ранното ти съобщение са 4")
	assert.Contains(t, out, "С твоя линк, който става (1):\n• A Tug of War (Lou, 2019) — PDF")
	assert.Contains(t, out, "Намерих пълния текст другаде (2)")
	assert.Contains(t, out, "• Maxing out (Ang, 2011) (линкът ти: страницата има само резюме")
	assert.Contains(t, out, "\n  → https://e.example/maxing.pdf (от търсенето)\n")
	assert.Contains(t, out, "• Sullivan, Timmermann, White (1999) — „Data‐Snooping, Technical Trading Rule Performance, and the Bootstrap“ (няма линк)\n"+
		"  → http://eprints.example/119144/1/dp303.pdf (от каталога OpenAlex)", "a reference with names only is found by its authors and year; Wiley refuses programs")
	assert.Contains(t, out, "Не намерих (1):\n• Smith (2004) — няма линк; без заглавие и с едно име не мога да я позная сигурно")
	assert.Contains(t, out, "чак след твоето „да“")
	assert.Contains(t, out, "Да започна ли?")
	assert.ElementsMatch(t, []string{
		"openalex: per-page=8&search=Maxing out",
		"Maxing out Ang pdf",
		"openalex: filter=raw_author_name.search:Sullivan,raw_author_name.search:Timmermann,raw_author_name.search:White,publication_year:1999&per-page=8&sort=cited_by_count:desc",
	}, *asked, "the catalogue first, the search when it has no free copy; nothing for a name alone")

	p, err := e.plans.Proposed(context.Background(), e.userID, "s")
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, taskrun.KindKeyFacts, p.Kind)
	require.Len(t, p.Steps, 3)
	assert.Equal(t, "https://a.example/paper.pdf", p.Steps[0].Link)
	assert.Empty(t, p.Steps[0].Expect, "the person's own link is theirs")
	assert.Equal(t, "A Tug of War", p.Steps[0].Title, "named by its title in the library")
	assert.Equal(t, "https://e.example/maxing.pdf", p.Steps[1].Link)
	assert.Equal(t, "Maxing out", p.Steps[1].Expect, "the runner checks that the document is this work")
	assert.Equal(t, "http://eprints.example/119144/1/dp303.pdf", p.Steps[2].Link)
	assert.Equal(t, "Data‐Snooping, Technical Trading Rule Performance, and the Bootstrap", p.Steps[2].Expect)
	assert.Equal(t, "Data‐Snooping, Technical Trading Rule Performance, and the Bootstrap", p.Steps[2].Title)
	assert.Equal(t, []string{"https://researchonline.example/id/eprint/119144/1/dp303.pdf"}, p.Steps[2].Also, "the other copy, tried when the first gives nothing")
	assert.NotContains(t, f.asked, "https://e.example/maxing.pdf", "a link that is not the person's is not opened before the yes")
	assert.NotContains(t, f.asked, "http://eprints.example/119144/1/dp303.pdf")

	start := streamedText(t, e.say(t, "s", "да"))
	assert.Contains(t, start, "Започнах")
}

func TestTheCatalogueTakesTheWorkOfTheReferenceAndItsFreeCopies(t *testing.T) {
	works := []scholarWork{
		{Title: "Maxed Out?", Year: 2014, Surnames: []string{"Crandall"}, PDFs: []string{"https://x.example/maxed.pdf"}},
		{Title: "Day Trading for a Living?", Year: 2019, Surnames: []string{"Chague", "De-Losso", "Giovannetti"}, Open: true},
		{Title: "Day trading for a living? Fernando", Year: 2019, Surnames: []string{"Chague", "De-Losso"},
			PDFs: []string{"https://doi.org/10.1/x", "https://repec.example/47WP.pdf#2", "https://onlinelibrary.wiley.com/doi/pdfdirect/1", "https://b.example/47WP.pdf"}},
	}
	day := reference{Title: "Day Trading for a Living?", Text: "Chague, De-Losso, Giovannetti, Day Trading for a Living?"}
	f := scholarPDFs(works, day)
	assert.Equal(t, "Day Trading for a Living?", f.Title)
	assert.Equal(t, []string{"https://repec.example/47WP.pdf", "https://b.example/47WP.pdf"}, f.PDFs,
		"the copies of another record of the same work; no DOI, no Wiley, no fragment")
	assert.True(t, f.Open)

	other := reference{Title: "Day Trading for a Living?", Text: "Aziz, Day Trading for a Living?"}
	assert.Equal(t, scholarFind{}, scholarPDFs(works, other), "the same title by another author is another work")

	cederburg := reference{Title: "On the Performance of Volatility-Managed Portfolios", Text: "Cederburg, O'Doherty, Wang, Yan"}
	f = scholarPDFs([]scholarWork{{Title: "On the performance of volatility-managed portfolios", Year: 2020,
		Surnames: []string{"Cederburg", "O’Doherty", "Wang"}}}, cederburg)
	assert.Equal(t, "On the performance of volatility-managed portfolios", f.Title, "the work is known")
	assert.Empty(t, f.PDFs, "but it has no free copy")
	assert.False(t, f.Open, "and none anywhere")

	george := reference{Text: "George, Hwang (2004) за 52-седмичния връх", Year: "2004"}
	f = scholarPDFs([]scholarWork{
		{Title: "Inverse modeling using multi-block PLS", Year: 2004, Surnames: []string{"Hwang", "Stephanopoulos"}},
		{Title: "The 52‐Week High and Momentum Investing", Year: 2004, Surnames: []string{"GEORGE", "Hwang"}},
	}, george)
	assert.Equal(t, "The 52‐Week High and Momentum Investing", f.Title, "names only: all of them, and the year")
	f = scholarPDFs([]scholarWork{{Title: "The 52‐Week High and Momentum Investing", Year: 2009, Surnames: []string{"George", "Hwang"}}}, george)
	assert.Empty(t, f.Title, "another year is another work")
}

func TestAClosedWorkIsNotLookedForElsewhere(t *testing.T) {
	closed := `{"results":[{"title":"On the performance of volatility-managed portfolios","publication_year":2020,"open_access":{"is_oa":false},
		"authorships":[{"author":{"display_name":"Scott Cederburg"}}]}]}`
	e, _, asked := sourcesEnvWith(t, map[string]string{
		"Volatility-Managed": `{"results":[{"title":"On the performance of volatility-managed portfolios","url":"https://slides.example/b37.pdf"}]}`,
	}, map[string]string{"search=On the Performance": closed})
	e.say(t, "s", "ето ги:\n- Cederburg, O'Doherty, [On the Performance of Volatility-Managed Portfolios](https://c.example/abstract) (JFE 2020).\n"+
		"- Lou, Polk, [A Tug of War](https://a.example/paper.pdf) (JFE 2019).")

	out := streamedText(t, e.say(t, "s", "потърси в интернет дали някой от източниците може да се извлече"))

	assert.Contains(t, out, "в каталога е, но е затворена (няма свободна версия) — копия другаде не търся")
	assert.NotContains(t, out, "slides.example")
	for _, q := range *asked {
		assert.True(t, strings.HasPrefix(q, "openalex: "), "no search after the catalogue said it is closed: %s", q)
	}
}

func TestTheLookupOfTheCatalogue(t *testing.T) {
	r := &DefaultToolRegistry{openAlexURL: "https://c.example/works"}
	assert.Equal(t, "https://c.example/works?per-page=8&search=Maxing+out", r.openAlexQuery(reference{Title: "Maxing out"}))
	assert.Equal(t, "https://c.example/works?filter=raw_author_name.search%3AGeorge%2Craw_author_name.search%3AHwang%2Cpublication_year%3A2004&per-page=8&sort=cited_by_count%3Adesc",
		r.openAlexQuery(reference{Text: "George, Hwang (2004) за 52-седмичния връх", Year: "2004"}))
	assert.Empty(t, r.openAlexQuery(reference{Text: "Smith (2004)", Year: "2004"}), "one name is not enough")
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
