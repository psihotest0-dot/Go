package taskrun

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"go-monolith/internal/db"
)

// The kinds of task the server walks: what it makes of each document.
const (
	// KindKeyFacts is a file with the key facts of each document (the owner's request of
	// 2026-11-21: "за всеки от добавените източници направи отделен файл с ключовите му факти").
	KindKeyFacts = "key_facts"
	// KindSummary is a file with a short summary and the main points of each document.
	KindSummary = "summary"
)

// Kinds are the kinds the server walks.
var Kinds = []string{KindKeyFacts, KindSummary}

const (
	// passageBudgetRunes bounds the passages of one document in a request: about 6 000 tokens of
	// English, 7 000 of Bulgarian, well inside the 16 384 of a slot of the local model with the
	// instructions and the answer.
	passageBudgetRunes = 14000
	// headShare is the part of the budget the start of the document takes (the abstract and the
	// introduction say what the document is); the rest goes to the passages nearest to the
	// recipe's question (the results, the conclusion).
	headShare = 0.45
	// minFacts: an answer with fewer lines of facts did not do the work.
	minFacts = 3
	// answerTokens bounds the answer of the model; ten facts are about 600 tokens of Bulgarian.
	answerTokens = 1400
)

// recipe is what the model is asked to write for one document.
type recipe struct {
	// fileSuffix ends the name of the file: "<document> - ключови факти.md".
	fileSuffix string
	// title heads the file.
	title string
	// query finds the passages that matter besides the start of the document.
	query string
	// instructions are the system message.
	instructions string
	// noun names the work in the person's words: "файл с ключовите факти".
	noun string
}

var recipes = map[string]recipe{
	KindKeyFacts: {
		fileSuffix: "ключови факти",
		title:      "Ключови факти",
		query:      "main results findings conclusion numbers effect evidence основни резултати изводи",
		noun:       "файл с ключовите факти",
		instructions: `Пишеш файл с ключовите факти на ЕДИН документ от библиотеката на потребителя. Имаш само откъсите от документа по-долу — нищо друго.
Правила:
- Напиши от 5 до 10 факта на български език, всеки на отделен ред, който започва с "- ".
- Всеки факт е взет от откъсите. Не добавяй нищо от своите знания и нищо, което го няма в откъсите.
- Всяко число (проценти, години, размери, резултати) препиши точно както е в откъса. Ако не си сигурен в някое число, не го пиши.
- Без увод, без заглавие и без заключение: само редовете с фактите.`,
	},
	KindSummary: {
		fileSuffix: "резюме",
		title:      "Резюме",
		query:      "main idea results conclusion summary основна идея резултати изводи",
		noun:       "файл с резюме",
		instructions: `Пишеш резюме на ЕДИН документ от библиотеката на потребителя. Имаш само откъсите от документа по-долу — нищо друго.
Правила:
- Първо 1 до 3 кратки абзаца на български: за какво е документът и какво казва.
- После ред "Основни точки:" и под него от 3 до 7 реда, всеки започва с "- ".
- Всичко е взето от откъсите. Не добавяй нищо от своите знания.
- Всяко число препиши точно както е в откъса. Ако не си сигурен в някое число, не го пиши.`,
	},
}

// pickPassages chooses the passages of a document for its request: all of it when it fits, else
// its start and the passages nearest to the recipe's question (near, the ids of the search), in
// the order of the document.
func pickPassages(chunks []db.DocumentChunk, near []string) []db.DocumentChunk {
	total := 0
	for _, c := range chunks {
		total += utf8.RuneCountInString(c.Text)
	}
	if total <= passageBudgetRunes {
		return chunks
	}
	chosen := map[int]bool{}
	used := 0
	take := func(i int, limit int) bool {
		n := utf8.RuneCountInString(chunks[i].Text)
		if chosen[i] || used+n > limit {
			return false
		}
		chosen[i] = true
		used += n
		return true
	}
	head := int(float64(passageBudgetRunes) * headShare)
	for i := range chunks {
		if !take(i, head) {
			break
		}
	}
	byID := map[string]int{}
	for i, c := range chunks {
		byID[c.ID.String()] = i
	}
	for _, id := range near {
		if i, ok := byID[id]; ok {
			take(i, passageBudgetRunes)
		}
	}
	// What room is left goes to the end of the document, the conclusion (all of it without a
	// search, when there is no embedder).
	for i := len(chunks) - 1; i >= 0; i-- {
		take(i, passageBudgetRunes)
	}
	out := make([]db.DocumentChunk, 0, len(chosen))
	for i, c := range chunks {
		if chosen[i] {
			out = append(out, c)
		}
	}
	return out
}

// passagesText is the user message of the request: the name of the document and its passages.
func passagesText(docName string, passages []db.DocumentChunk) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Документ: %s\n\nОткъси от документа (по реда в него):\n", docName)
	for i, p := range passages {
		fmt.Fprintf(&b, "\n[откъс %d]\n%s\n", i+1, strings.TrimSpace(p.Text))
	}
	return b.String()
}

var (
	// numberRe is a number as it is written: 12, 0.41, 1,234.5, 3,5.
	numberRe = regexp.MustCompile(`\d+(?:[.,]\d+)*`)
	// listMarker is the number of a numbered line ("1. ", "2) "), which is no number of the text.
	listMarker = regexp.MustCompile(`(?m)^\s*\d{1,2}[.)]\s`)
	// factLine is a line of a fact: "- ", "* ", "• " or a numbered line.
	factLine = regexp.MustCompile(`^\s*(?:[-*•]|\d{1,2}[.)])\s+\S`)
)

// numberForms are the ways a number may be written for the same value: "1,5" and "1.5", "1,234"
// and "1234", "0.50" and "0.5".
func numberForms(tok string) []string {
	dot := strings.ReplaceAll(tok, ",", ".")
	bare := strings.NewReplacer(",", "", ".", "").Replace(tok)
	forms := []string{tok, dot, bare}
	if strings.Contains(dot, ".") && strings.Count(dot, ".") == 1 {
		trimmed := strings.TrimRight(strings.TrimRight(dot, "0"), ".")
		forms = append(forms, trimmed)
	}
	return forms
}

// numbersIn is every form of every number written in the texts.
func numbersIn(texts ...string) map[string]bool {
	out := map[string]bool{}
	for _, t := range texts {
		for _, tok := range numberRe.FindAllString(t, -1) {
			for _, f := range numberForms(tok) {
				out[f] = true
			}
		}
	}
	return out
}

// unsupportedNumbers are the numbers of text that none of the known ones is: numbers the model
// did not take from the passages.
func unsupportedNumbers(text string, known map[string]bool) []string {
	var out []string
	for _, tok := range numberRe.FindAllString(listMarker.ReplaceAllString(text, ""), -1) {
		ok := false
		for _, f := range numberForms(tok) {
			if known[f] {
				ok = true
				break
			}
		}
		if !ok && !slices.Contains(out, tok) {
			out = append(out, tok)
		}
	}
	return out
}

// checked is the answer of the model after the check of its numbers.
type checked struct {
	text    string
	facts   int
	dropped int // lines left out because a number of theirs is not in the passages
}

// checkAnswer keeps the lines of the answer whose numbers are all in the passages; a line with a
// number from elsewhere is left out (a fact with a wrong number is worse than none).
func checkAnswer(answer string, known map[string]bool) checked {
	var kept []string
	var c checked
	for _, line := range strings.Split(strings.ReplaceAll(answer, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if len(kept) > 0 && kept[len(kept)-1] != "" {
				kept = append(kept, "")
			}
			continue
		}
		if len(unsupportedNumbers(trimmed, known)) > 0 {
			c.dropped++
			continue
		}
		if factLine.MatchString(trimmed) {
			c.facts++
		}
		kept = append(kept, strings.TrimRight(line, " \t"))
	}
	c.text = strings.TrimSpace(strings.Join(kept, "\n"))
	return c
}

// cleanAnswer takes off what a model puts around an answer: a fence, a heading it was told not to
// write, the thinking of a model that thinks anyway.
func cleanAnswer(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "</think>"); i >= 0 {
		s = strings.TrimSpace(s[i+len("</think>"):])
	}
	s = strings.TrimPrefix(s, "```markdown")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
