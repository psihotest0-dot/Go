package jobs

import (
	"log/slog"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"go-monolith/metrics"
)

// The reflector also writes what the person did not say: an inference ("Потребителят не разбира
// кодирането достатъчно добре"), a signature under a quote read as his name. Its own measure of
// how sure it is does not help - in the night measurement of 2026-11-22 Gemma gave "high" to all
// 38 candidates, the guesses too. So the model is asked for the words of the person a fact rests
// on (evidence), and the code keeps a fact only when those words are in the message (plan step
// 0.2, 2026-11-22). A name is kept only when it is short, is itself in the message and its
// evidence is an introduction: the prompt says that a signature under a quote is not one, and in
// the measurement the model took "…мълчейки -Ивайло Иванов" for the name all the same.

// minEvidenceRunes: evidence shorter than this ("аз", "да") does not show where a fact comes from.
const minEvidenceRunes = 4

// maxNameWords bounds a name: more words is a phrase, not a name.
const maxNameWords = 3

// introduction is a person giving the name, in the text as normalizeForEvidence leaves it.
var introduction = regexp.MustCompile(`(^|[^\p{L}])(казвам се|името ми е|аз съм|наричай ме|наричайте ме|викай ми|викат ми|наричат ме|my name is|i am|i'm|call me)( |$)`)

// The reasons a fact is dropped (metrics.MemoryFactsDropped).
const (
	dropNoEvidence  = "no_evidence"
	dropNotInText   = "not_in_message"
	dropNameNotName = "name_not_a_name"
)

// normalizeForEvidence is the text as it is compared: lower case, one kind of quote and dash,
// whitespace as single spaces. The model copies a phrase but now and then changes „“ to "" or a
// dash, or joins two lines; that is still the person's words.
func normalizeForEvidence(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch r {
		case '„', '“', '”', '«', '»', '\'', '‘', '’', '`':
			r = '"'
		case '—', '–', '‑', '−':
			r = '-'
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// trimEvidence drops the punctuation around a quoted phrase ("…живея в Бремен." -> "живея в Бремен").
func trimEvidence(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSpace(r) || unicode.IsSymbol(r) })
}

// dropReason tells why a fact cannot stand on the message, or "" when it can.
func dropReason(message string, fact Fact) string {
	evidence := trimEvidence(normalizeForEvidence(fact.Evidence))
	if utf8.RuneCountInString(evidence) < minEvidenceRunes {
		return dropNoEvidence
	}
	text := normalizeForEvidence(message)
	if !strings.Contains(text, evidence) {
		return dropNotInText
	}
	if strings.EqualFold(strings.TrimSpace(fact.FactKey), "name") {
		name := trimEvidence(normalizeForEvidence(fact.Value))
		if name == "" || utf8.RuneCountInString(name) > maxNameRunes || len(strings.Fields(name)) > maxNameWords ||
			!strings.Contains(text, name) || !introduces(evidence, name) {
			return dropNameNotName
		}
	}
	return ""
}

// groundedFacts are the facts whose evidence is in the message. The dropped ones are counted and
// logged by reason only: their text is personal.
func groundedFacts(message string, facts []Fact) []Fact {
	kept := make([]Fact, 0, len(facts))
	for _, fact := range facts {
		if reason := dropReason(message, fact); reason != "" {
			metrics.MemoryFactsDropped.WithLabelValues(reason).Inc()
			slog.Info("a fact of the reflector was dropped: it does not stand on the person's words", "reason", reason, "fact_key", fact.FactKey)
			continue
		}
		kept = append(kept, fact)
	}
	return kept
}

// introduces tells whether the evidence of a name introduces the person by it: the name right after
// an introduction ("казвам се Ивайло"), not "-Ивайло Иванов" under a quote and not another name
// later in the sentence ("казвам се Ивайло, а брат ми е Петър" does not give "Петър").
func introduces(evidence, name string) bool {
	for _, loc := range introduction.FindAllStringIndex(evidence, -1) {
		if strings.HasPrefix(strings.TrimLeft(evidence[loc[1]:], " "), name) {
			return true
		}
	}
	return false
}
