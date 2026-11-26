package agent

import (
	"regexp"
	"strings"
	"unicode/utf8"

	openai "github.com/sashabaranov/go-openai"
)

// "Изброй всички" after an answer from the internet searches again (2026-11-26). In Telegram on
// 2026-11-25 the owner asked which Champions League matches were on that day; the answer, from a
// search whose results named all nine, listed four. "Това ли са всички за днес?" got "Да" and
// "Искам да ми изброиш всички мачове" got the same four as "пълния списък" - with no new search.
// Replayed on the real model with the history of that turn: 10 of 10 answers to "изброй всички"
// called no tool and called the four "всички"; a note in the search result could not help, since
// no search was made (go-monolith-backups\measurements\matches_search_2026-11-26). A message that
// asks for the whole of a list, or for what it lacks, right after an answer from a web search is
// now one the first round must search for, as "провери в интернет" is (forcedFirstTool).

const (
	// wholeListMaxRunes: "Искам да ми изброиш всички мачове от лигата за днес" is short; a long
	// message is a new request.
	wholeListMaxRunes = 200
	// webFollowupAnswers is how many answers back a web search still counts.
	webFollowupAnswers = 3
)

// wholeListWords: the whole of a list, or what it lacks.
var wholeListWords = regexp.MustCompile(`(?i)(?:^|\P{L})(?:всички|всичките|целия|целият|пълния|пълният|пълен|останалите|другите|липсва|липсват|пропусна|пропуснала|пропускаш|само\s+\d+|само\s+(?:два|две|три|четири|пет|шест)|all|complete|full\s+list|the\s+rest|missing)(?:\P{L}|$)`)

// webSearchRecord is the record of a web search next to an answer in the history (tool_log.go).
const webSearchRecord = "web_search \""

// asksForTheWholeList tells whether a message asks for the whole of a list or for what it lacks.
func asksForTheWholeList(message string) bool {
	message = strings.TrimSpace(withoutClosingNote(message))
	return message != "" && utf8.RuneCountInString(message) <= wholeListMaxRunes && wholeListWords.MatchString(message)
}

// recentAnswerSearchedWeb tells whether one of the last answers before the person's message was
// made from a web search: the history shows the record of its tools next to it.
func recentAnswerSearchedWeb(messages []openai.ChatCompletionMessage) bool {
	seenUser, answers := false, 0
	for i := len(messages) - 1; i >= 0 && answers < webFollowupAnswers; i-- {
		switch messages[i].Role {
		case openai.ChatMessageRoleUser:
			seenUser = true
		case openai.ChatMessageRoleAssistant:
			if !seenUser {
				continue
			}
			answers++
			c := messages[i].Content
			if at := strings.LastIndex(c, toolLogNoteHead); at >= 0 && strings.Contains(c[at:], webSearchRecord) {
				return true
			}
		}
	}
	return false
}

// searchesWebAgain tells whether the first round must call web_search: the message asks for the
// whole of a list right after an answer from the internet, and the tool is on offer.
func searchesWebAgain(tools []openai.Tool, messages []openai.ChatCompletionMessage) bool {
	return onlyTool(tools, "web_search") != nil && asksForTheWholeList(lastUserContent(messages)) && recentAnswerSearchedWeb(messages)
}
