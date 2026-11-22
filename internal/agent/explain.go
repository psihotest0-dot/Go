package agent

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/retrieval"
	"go-monolith/metrics"
)

// The explanation from the library (plan step 2.8): "не разбирам какво са горутините", "обясни ми
// каналите в Go". The code gathers the passages - the model only writes the explanation from them.
//
// Measured on 2026-11-22 (go-monolith-backups\measurements\explain_2026-11-22): 15 such questions on
// the owner's books. The dossier of the code - the search of the model and its English twin, and
// seven searches made from templates ("X е", "X пример", "защо X", "как работи X", "what is X",
// "X example", "how does X work"), at most two passages of a book, the text around the first three -
// brought a little more of what explains the subject than today's search (a blind DeepSeek judge:
// 131 against 123 points, 44 against 41 passages that explain). With the recipe "what it is, how it
// works, an example, which book says what", the owner, blind, liked the dossier's answer in 8 of 8
// questions and today's in 0 of 8 - the local Gemma and DeepSeek alike (each 8 of 8).
//
// So:
//   - prepareTurn asks explainSubject for the message; a turn with a subject is an explanation;
//   - the first round must search the library (it is forced, as a question to the books is);
//   - the first search_documents of the turn returns the dossier with the recipe as its note;
//   - the model of the turn is EXPLAIN_LLM's: the local one unless it says deepseek (the
//     owner's button of a conversation - cloud or home - still decides first, model_routing.go);
//   - an explanation gets no plan of an answer (answer_plan.go): the two notes would compete.
//
// Off unless EXPLAIN_DOSSIER_ENABLED (WithExplainDossier).
//
// The measured recipe asked for the number of a passage after a sentence ("[2]"); here it does
// not. The marks had to be taken out of the answer, and an explanation of code has "a[1]" and
// "var a [3]int" in it, which the filter of the marks would have broken.

const (
	// explainPerFile and explainPassages are the shape of the dossier: at most two passages of
	// a book, eight in all (as the search of today gives them).
	explainPerFile  = 2
	explainPassages = maxDocumentSearchResults
	// explainWithNeighbours is how many of the best passages bring the text around them.
	explainWithNeighbours = 3
	// explainRunes and explainNeighbourRunes bound the content of a passage in the dossier:
	// a passage alone, and one with its neighbours (as measured).
	explainRunes          = 1500
	explainNeighbourRunes = 4500
	// explainSearchesAtOnce: the searches of the dossier run side by side, this many at a time
	// (each embeds its query and asks the database).
	explainSearchesAtOnce = 3
)

// Results of the dossier, the labels of explain_dossier_total.
const (
	explainBuilt  = "built"
	explainEmpty  = "empty"  // no search found anything
	explainFailed = "failed" // every search failed
	explainScoped = "scoped" // the question names a file: the search stays in it, no dossier
)

var (
	// explainOpening: the start of a request for an explanation, after the little words a speaker
	// puts in front of it. The subject is what follows. Only the start counts - "Това много не го
	// разбирам да ти кажа" (one of the owner's real messages, about Lili) does not start with it.
	explainOpening = regexp.MustCompile(`(?i)^\s*(?:(?:а|и|ами|добре|моля|хайде|е|но|please)[,\s]+)?(?:` +
		`не\s+(?:го\s+|ги\s+|я\s+)?разбирам|обясн(?:и|ете)|разясн(?:и|ете)|` +
		`какво\s+(?:е|са|значи|означава|означават|представлява|представляват|прави|правят)|` +
		`как\s+работ(?:и|ят)|` +
		`(?:i\s+)?(?:don'?t|do\s+not)\s+understand|explain|what\s+(?:is|are)|how\s+(?:does|do)` +
		`)(?:[^\p{L}]|$)`)

	// explainLead: what may stand between the opening and the subject: "обясни МИ", "не разбирам
	// КАКВО СА", "не разбирам КАК РАБОТИ". Taken off again and again.
	explainLead = regexp.MustCompile(`(?i)^(?:(?:ми|ни|me|to\s+me)\s+|` +
		`(?:какво|как|защо|what|how|why)(?:\s+(?:е|са|значи|означава|представлява|представляват|работи|работят|прави|правят|is|are|does|do))?\s+|` +
		`(?:a|an|the)\s+)`)

	// explainTail: the end of a subject that is not the subject: ", не ги разбирам", "и с какво се
	// различава от виртуална машина", ", за което пише в ...", the mark of the question.
	explainTail = regexp.MustCompile(`(?i)(?:\s*[,;:(].*|\s+(?:и|and)\s+(?:с\s+)?(?:какво|как|защо|кога|what|how|why)\s.*|\s+work)?[\s?!.…]*$`)

	// explainNotSubject: the first word of a "subject" that points back, or asks for more of
	// the same - "обясни ми го по-просто", "обясни ми пак", "explain it again" - is not a new
	// subject: such a turn is a follow-up, and the search of today handles it (earlier_answer.go).
	explainNotSubject = regexp.MustCompile(`(?i)^(?:го|ги|я|ме|те|това|тези|този|тази|онова|него|нея|тях|пак|отново|още|по-\p{L}*|подробно|накратко|просто|` +
		`it|this|that|them|these|those|again|more|briefly|simply)(?:[^\p{L}]|$)`)

	// explainEnglishLead and explainEnglishTail take the subject out of the English query of the
	// model ("what are goroutines in Go" -> "goroutines in Go").
	explainEnglishLead = regexp.MustCompile(`(?i)^\s*(?:(?:what|how)\s+(?:is|are|does|do)\s+(?:a\s+|an\s+|the\s+)?|explain\s+(?:the\s+)?|define\s+|meaning\s+of\s+)`)
	explainEnglishTail = regexp.MustCompile(`(?i)(?:\s+(?:work|do))?[\s?!.…]*$`)
)

// explainSubject is the subject of a request for an explanation ("горутините в Go" of "не разбирам
// какво са горутините в Go"), or "" when the message is not one. It holds to the gates of a
// question to the books (asksForKnowledge): a few words at least, not long, not about the two who
// talk, not about the news.
func explainSubject(message string) string {
	message = strings.TrimSpace(message)
	if utf8.RuneCountInString(message) > maxKnowledgeQuestionRunes || len(strings.Fields(message)) < minKnowledgeQuestionWords {
		return ""
	}
	if shouldSkipRetrieval(message) || conversationalWords.MatchString(message) || currentAffairs.MatchString(message) {
		return ""
	}
	loc := explainOpening.FindStringIndex(message)
	if loc == nil {
		return ""
	}
	subject := strings.TrimSpace(message[loc[1]:])
	for {
		rest := strings.TrimSpace(explainLead.ReplaceAllString(subject, ""))
		if rest == subject {
			break
		}
		subject = rest
	}
	subject = strings.TrimSpace(explainTail.ReplaceAllString(subject, ""))
	if subject == "" || explainNotSubject.MatchString(subject) || letters(subject) < 3 {
		return ""
	}
	return subject
}

// englishSubject is the subject in the English query of the model ("what does Kubernetes do" ->
// "Kubernetes", "what is a Docker container and how is it different from ..." -> "Docker
// container"), "" when there is none.
func englishSubject(queryEN string) string {
	s := explainEnglishLead.ReplaceAllString(strings.TrimSpace(queryEN), "")
	s = strings.TrimSpace(explainTail.ReplaceAllString(s, ""))
	s = strings.TrimSpace(explainEnglishTail.ReplaceAllString(s, ""))
	if letters(s) < 3 {
		return ""
	}
	return s
}

func letters(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			n++
		}
	}
	return n
}

// cyrillic tells whether a text is written mostly in Cyrillic.
func cyrillic(s string) bool {
	c, l := 0, 0
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Cyrillic, r):
			c++
		case unicode.Is(unicode.Latin, r):
			l++
		}
	}
	return c > l
}

// explainQueries are the searches of the dossier: the model's query and its English twin first,
// then the templates of the subject - the Bulgarian ones when the person wrote in Bulgarian (the
// subject may be "Kubernetes" all the same), the English ones for the English subject (from the
// model's English query; the subject itself when the person wrote in English). Each query once.
func explainQueries(query, queryEN, subject string, bulgarian bool) []string {
	var bg, en string
	if bulgarian {
		bg, en = subject, englishSubject(queryEN)
	} else {
		en = subject
	}
	list := []string{query, queryEN}
	if bg != "" {
		list = append(list, bg+" е", bg+" пример", "защо "+bg, "как работи "+bg)
	}
	if en != "" {
		list = append(list, "what is "+en, en+" example", "how does "+en+" work")
	}
	seen := map[string]bool{}
	var out []string
	for _, q := range list {
		q = strings.TrimSpace(q)
		key := strings.ToLower(q)
		if q == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, truncateRunes(q, maxQueryENRunes))
	}
	return out
}

// explainRecipe is the note of the dossier: how the model is to write the explanation.
func explainRecipe(bulgarian bool) string {
	language := "Bulgarian"
	if !bulgarian {
		language = "the language of the person's message"
	}
	return "These passages were gathered from the user's books to explain the subject to the person, " +
		"who said they do not understand it. Explain it simply, in " + language + ": what it is, how it works, " +
		"and one short example. Connect two or three of the sources and say which book says what. " +
		"Take the facts only from the passages."
}

// pickDossier is the dossier of the merged results of its searches (the best score first): at
// most explainPerFile passages of a file, explainPassages in all.
func pickDossier(merged []retrieval.SearchResult) []retrieval.SearchResult {
	perFile := map[string]int{}
	var out []retrieval.SearchResult
	for _, r := range merged {
		if perFile[r.FileName] >= explainPerFile {
			continue
		}
		perFile[r.FileName]++
		out = append(out, r)
		if len(out) == explainPassages {
			break
		}
	}
	return out
}

// explainSearches runs the searches side by side. It returns their results in the order of the
// queries; an error only when every search failed.
func (r *DefaultToolRegistry) explainSearches(ctx context.Context, userID uuid.UUID, queries []string) ([][]retrieval.SearchResult, error) {
	lists := make([][]retrieval.SearchResult, len(queries))
	errs := make([]error, len(queries))
	slots := make(chan struct{}, explainSearchesAtOnce)
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			lists[i], errs[i] = r.retrieval.SearchRawChunks(ctx, userID, q, maxDocumentSearchResults)
		}()
	}
	wg.Wait()
	failed := 0
	for i, err := range errs {
		if err != nil {
			failed++
			slog.WarnContext(ctx, "a search of the explanation dossier failed", "query", queries[i], "error", err)
		}
	}
	if failed == len(queries) {
		return nil, fmt.Errorf("failed to search documents: %w", errs[0])
	}
	return lists, nil
}

// neighbours is the text before and after a passage in its file ("" where there is none).
func (r *DefaultToolRegistry) neighbours(ctx context.Context, userID, id uuid.UUID) (before, after string) {
	rows, err := r.db.Queries.GetChunkNeighbours(ctx, db.GetChunkNeighboursParams{ID: id, UserID: userID})
	if err != nil {
		slog.WarnContext(ctx, "could not read the text around a passage of the dossier", "error", err)
		return "", ""
	}
	for _, row := range rows {
		switch row.Delta {
		case -1:
			before = row.Text
		case 1:
			after = row.Text
		}
	}
	return before, after
}

// explainDossier is the result of search_documents for an explanation (see the top of the file).
func (r *DefaultToolRegistry) explainDossier(ctx context.Context, userID uuid.UUID, query, queryEN, subject string) (interface{}, error) {
	// The language of the person: the message of the turn (the subject alone may be "Docker").
	bulgarian := cyrillic(currentQuery(ctx)) || cyrillic(subject)
	lists, err := r.explainSearches(ctx, userID, explainQueries(query, queryEN, subject, bulgarian))
	if err != nil {
		metrics.ExplainDossiers.WithLabelValues(explainFailed).Inc()
		return nil, err
	}
	picked := pickDossier(mergePassages(0, lists...))
	if len(picked) == 0 {
		metrics.ExplainDossiers.WithLabelValues(explainEmpty).Inc()
		return map[string]interface{}{"status": "success", "results": []interface{}{}}, nil
	}
	metrics.ExplainDossiers.WithLabelValues(explainBuilt).Inc()
	turnFrom(ctx).markExternal(sourceSearchDocuments)

	found := make([]api.SourceInfo, 0, len(picked))
	results := make([]map[string]interface{}, 0, len(picked))
	for i, res := range picked {
		found = append(found, passageSource(res))
		content := truncateString(res.Content, explainRunes)
		if i < explainWithNeighbours {
			before, after := r.neighbours(ctx, userID, res.ID)
			if before != "" || after != "" {
				content = truncateString(strings.Join(nonEmpty(before, res.Content, after), " "), explainNeighbourRunes)
			}
		}
		results = append(results, map[string]interface{}{
			"n":         i + 1,
			"file_name": res.FileName,
			"content":   content,
		})
	}
	turnFrom(ctx).addSources(found)
	return map[string]interface{}{
		"status":  "success",
		"note":    explainRecipe(bulgarian),
		"results": results,
	}, nil
}

func nonEmpty(parts ...string) []string {
	out := parts[:0:0]
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// WithExplainDossier turns on the explanation from the library; cloud sends such a turn to the
// paid model (EXPLAIN_LLM=deepseek), when there is one.
func (o *Orchestrator) WithExplainDossier(cloud bool) *Orchestrator {
	o.explainDossier, o.explainCloud = true, cloud
	for _, r := range []string{explainBuilt, explainEmpty, explainFailed, explainScoped} {
		metrics.ExplainDossiers.WithLabelValues(r)
	}
	return o
}

// explainSubjectFor is the subject of an explanation in this message, "" when the dossier is off
// or the message asks for none.
func (o *Orchestrator) explainSubjectFor(message string) string {
	if !o.explainDossier {
		return ""
	}
	return explainSubject(withoutClosingNote(message))
}

func (t *turnState) setExplain(subject string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.explain = subject
}

// explaining tells whether the turn is an explanation from the library.
func (t *turnState) explaining() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.explain != ""
}

// takeExplain is the subject for the dossier, once a turn: a second search of the turn is an
// ordinary one.
func (t *turnState) takeExplain() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.explainDone {
		return ""
	}
	t.explainDone = true
	return t.explain
}
