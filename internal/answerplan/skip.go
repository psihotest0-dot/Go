package answerplan

import (
	"regexp"
	"strings"
	"unicode"
)

// Why a turn gets no plan before its message is even measured. The values are the labels of
// the metric answer_plan_total (with the "skipped_" prefix).
const (
	SkipShort      = "short"       // fewer than minLetters letters
	SkipGreeting   = "greeting"    // a greeting, a thank-you, a farewell, "ок", "продължи"...
	SkipLink       = "link"        // a link: the message is about the page
	SkipCode       = "code"        // a code block
	SkipForcedTool = "forced_tool" // the turn starts with a forced tool (books, a passage)
	SkipFile       = "file"        // asks for a file to be written or removed
	SkipAttachment = "attachment"  // a file is attached to the conversation
	SkipStatus     = "status"      // asks how far some work is ("успя ли?")
	SkipCloud      = "cloud"       // the turn goes to the paid model: the memories stay at home
	SkipTask       = "task"        // the conversation has a task plan in progress: a task, not a talk
)

// Signals are what the turn knows about the message besides its words; the agent sets them
// (forcedFirstTool, asksToWrite/asksToRemoveFile, the attachments, asksForStatus, the router).
//
// Cloud is a turn the router sends to the paid model (the owner's choice for the conversation;
// a library question goes there too, but ForcedTool already leaves it out). A plan puts the
// person's memories into the note of the turn, and the owner's decision is that they do not
// leave the machine (2026-11-22).
type Signals struct {
	Cloud          bool
	Task           bool
	ForcedTool     bool
	FileRequest    bool
	Attachment     bool
	StatusQuestion bool
}

// minLetters is the fewest letters a message needs to be measured. The rule of the first
// design, "under about 12 characters", would have dropped "уморен съм" (10), which scores
// +0.075 - far above the threshold - while the short messages that are not personal ("Как си
// Лили", "Здрасти", "опитай пак", "/start") all score -0.013 or less (2026-11-22). So the
// detector decides on short messages; only the ones with almost no words are not measured.
const minLetters = 3

// minLettersBesidesFarewell is how many other letters a message with a farewell in it needs
// to be measured: the person is leaving, and a planned reply to "не знам още, до скоро" (a
// farewell of the owner's, scored at the threshold) would talk to no one; "Ще си почивам
// малко за сега. Ще се чуем пак" (personal, by the owner's labels) is measured.
const minLettersBesidesFarewell = 12

// courtesyPhrases are the words of a reply that say nothing about the person - greetings,
// thanks, consent and the short commands of a task - and farewellPhrases the ones that end a
// conversation. A message of nothing else is not measured; "Добро утро, уморен съм" is.
var (
	courtesyPhrases = phrases("добро утро", "добър ден", "добър вечер", "здравейте", "здравей",
		"здрасти", "привет", "хей", "ало", "благодаря", "мерси", "окей", "ок", "добре", "продължи",
		"продължавай", "давай", "започни", "hello", "hi", "thanks", "ok")
	farewellPhrases = phrases("лека нощ", "до скоро", "до утре", "ще се чуем", "чао", "довиждане", "bye")
)

// phrases matches any of the phrases as whole words, in any case.
func phrases(list ...string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(?:^|[^\p{L}])(` + strings.Join(list, "|") + `)(?:$|[^\p{L}])`)
}

var linkPattern = regexp.MustCompile(`(?i)https?://|www\.`)

// SkipReason is why a message gets no plan whatever its score, or "" when the detector should
// measure it.
func SkipReason(text string, s Signals) string {
	switch {
	case s.Cloud:
		return SkipCloud
	case s.Task:
		return SkipTask
	case s.ForcedTool:
		return SkipForcedTool
	case s.FileRequest:
		return SkipFile
	case s.Attachment:
		return SkipAttachment
	case s.StatusQuestion:
		return SkipStatus
	case strings.Contains(text, "```"):
		return SkipCode
	case linkPattern.MatchString(text):
		return SkipLink
	}
	if letters(text) < minLetters {
		return SkipShort
	}
	rest, farewell := without(text, farewellPhrases)
	rest, _ = without(rest, courtesyPhrases)
	if letters(rest) == 0 || farewell && letters(rest) < minLettersBesidesFarewell {
		return SkipGreeting
	}
	return ""
}

// without is text without the phrases of re, and whether it had any.
func without(text string, re *regexp.Regexp) (string, bool) {
	found := false
	for {
		loc := re.FindStringSubmatchIndex(text)
		if loc == nil {
			return text, found
		}
		found = true
		text = text[:loc[2]] + " " + text[loc[3]:]
	}
}

func letters(text string) int {
	n := 0
	for _, r := range text {
		if unicode.IsLetter(r) {
			n++
		}
	}
	return n
}
