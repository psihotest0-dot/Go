package agent

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// A message about an attached file that is not shown in the prompt reads it first, and a question
// about the news searches first (2026-11-26). In the Flet chat of that day the owner attached
// main.py (507 lines: too big to be shown in the prompt, which lists it as "not shown here") and
// asked "какво мислиш за този код": the answer described code "for crypto mining" that is not in
// the file, and "закачен е към чата, но не мисля, че го виждаш" got an apology - no read_attachment
// either time, although the prompt says never to say a file cannot be seen before trying the tool.
// "Какви са днешните водещи световни новини" got news made up, with no search. Replayed on the real
// model with that history, 5 times each: 0 of 15 called a tool ("Не си прикачил никакъв код" 5 of
// 5; made-up news 5 of 5) (go-monolith-backups\measurements\attach_flet_2026-11-26).

// attachmentAskMaxRunes: "какво мислиш за този код" is short; a long message is a new request.
const attachmentAskMaxRunes = 300

var (
	// attachmentWords: the code, the attached file, the file itself.
	attachmentWords = regexp.MustCompile(`(?i)(?:^|\P{L})(?:код|кода|кодът|кодa|скрипт\p{L}*|програмата|прикач\p{L}*|закач\p{L}*|закъч\p{L}*|файла|файлът|файлa|този\s+файл|тоя\s+файл|моя\s+файл|code|script|attach\p{L}*|the\s+file|this\s+file)(?:\P{L}|$)`)
	// newsWords: the news of the day.
	newsWords = regexp.MustCompile(`(?i)(?:^|\P{L})(?:новини|новините|новина|новината|news|headlines|какво\s+(?:се\s+)?(?:случва|става)\s+(?:по|в)\s+света)(?:\P{L}|$)`)
)

// asksAboutAttachment tells whether a message is about an attached file: its words, or the name
// of one of the files. A request to write a file is not one: the file tools answer it.
func asksAboutAttachment(message string, names []string) bool {
	message = strings.TrimSpace(withoutClosingNote(message))
	if message == "" || utf8.RuneCountInString(message) > attachmentAskMaxRunes || asksToWrite(message) {
		return false
	}
	if attachmentWords.MatchString(message) {
		return true
	}
	lower := strings.ToLower(message)
	for _, name := range names {
		if name != "" && strings.Contains(lower, strings.ToLower(name)) {
			return true
		}
	}
	return false
}

// asksForNews tells whether a message asks for the news, which only a search knows.
func asksForNews(message string) bool {
	message = strings.TrimSpace(message)
	return utf8.RuneCountInString(message) <= maxKnowledgeQuestionRunes && newsWords.MatchString(message)
}

// setHiddenAttachments records the attached files of the turn that the prompt does not show in full.
func (t *turnState) setHiddenAttachments(names []string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.hiddenAttachments = names
}

// hiddenAttachmentNames are the attached files of the turn that the prompt does not show in full.
func (t *turnState) hiddenAttachmentNames() []string {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.hiddenAttachments
}

// readsAttachmentFirst tells whether the first round must call read_attachment: the message is
// about an attached file and some attached file is not shown in the prompt.
func readsAttachmentFirst(t *turnState, message string) bool {
	names := t.hiddenAttachmentNames()
	return len(names) > 0 && asksAboutAttachment(message, names)
}
