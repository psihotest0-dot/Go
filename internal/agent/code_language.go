package agent

import "regexp"

// A search of the books about attached code gets the language of the code (2026-11-28). In the Flet
// chat of 2026-11-26 "кажи ми според книгите как е по-добре да го преструктурирам" about main.py
// read the file and searched the library, but the model's query had no language ("software
// engineering principles clean code architecture"): the passages came from books on FastAPI, Node.js,
// Rust, Go and hacking. The three queries of that turn against the owner's library, read-only
// (go-monolith-backups\measurements\books_code_lang_2026-11-28): passages from books on Python 4 of
// 24 as written, 18 of 24 with " Python" at the end, 14 of 24 with "Python " in front.

var (
	// programmingWords: a query about writing software, in English or Bulgarian.
	programmingWords = regexp.MustCompile(`(?i)(?:^|\P{L})(?:code|codes|coding|codebase|software|programming|refactor\p{L}*|architecture|design\s+patterns?|functions?|modules?|api|apis|framework\p{L}*|debug\p{L}*|` +
		`код|кода|кодът|програмир\p{L}*|софтуер\p{L}*|рефактор\p{L}*|архитектур\p{L}*|функци\p{L}*|модул\p{L}*)(?:\P{L}|$)`)
	// languageNames: a query that already names a language (or a runtime or a framework of one)
	// is left as it is, whichever language it names.
	languageNames = regexp.MustCompile(`(?i)(?:^|[^\p{L}#+])(?:python|golang|go|rust|javascript|typescript|node\.?js|node|java|kotlin|c#|c\+\+|ruby|php|swift|dart|scala|lua|bash|powershell|sql|flet|django|flask|fastapi|react)(?:[^\p{L}#+]|$)`)
)

// attachedCodeLanguage is the language of the attached code files, or "" when none of the files is
// code or they are in more than one language.
func attachedCodeLanguage(names []string) string {
	language := ""
	for _, name := range names {
		// The table of the analysis of a file (analyze.go).
		lang, _ := languageOf(name)
		if lang == "" {
			continue
		}
		if language != "" && language != lang {
			return ""
		}
		language = lang
	}
	return language
}

// withCodeLanguage adds the language to a query about programming that names none.
func withCodeLanguage(query, language string) string {
	if language == "" || query == "" || !programmingWords.MatchString(query) || languageNames.MatchString(query) {
		return query
	}
	return query + " " + language
}

// setCodeLanguage records the language of the code attached to the conversation (attachedCodeLanguage).
func (t *turnState) setCodeLanguage(language string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.codeLanguage = language
}

// attachedLanguage is the language of the code attached to the conversation, or "".
func (t *turnState) attachedLanguage() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.codeLanguage
}
