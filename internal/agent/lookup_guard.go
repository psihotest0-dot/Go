package agent

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"go-monolith/metrics"
)

// The model says it looked when it did not.
//
// In the owner's Telegram conversation of 2026-11-24 (21:27-21:36 UTC) four answers in a row
// said where they had looked - "Търсих в библиотеката ти по темата „OCR“…", "Проверих в книгите
// ти по Python…", "Намерих няколко източника…" (asked to look on the internet), "Проверих папката
// ти с файлове…" - and the next one "ще потърся в интернет…". Each turn was one request to the
// model and no tool was called: nothing was searched, and "не откривам" was the model's guess. The
// forced search (forced_tool.go) did not catch the messages ("Тема OCR", "Във книгите за python
// няма ли…" open with no question word), and each false "търсих" made the next more likely, as
// the history keeps the words of a turn and not its tool calls (the same as the files, P52).
//
// So an answer that says it searched, checked or found something in the library, on the internet
// or in the folder of files, or that it will look there now, in a turn that called no tool, is
// asked again once with the tools of that place offered and a call required (the tools of the
// mode of that place, turn_mode.go); what is kept is the second answer. Where those tools are not
// on offer, a line under the answer says that nothing was searched (answerChecks).
//
// The library, as a place things are put (2026-11-28): "ето източниците: <два линка>" got "Ще ги добавя
// веднага към библиотеката ти" in the live check of that day, and "Добавих тези източници към
// библиотеката ти" in 1 of 3 replays after a talk about inflation - with no call of add_to_library.
// Such an answer is asked again with add_to_library required, where it is on offer (the person
// wrote a link).
//
// Besides, a message that asks to look on the internet ("опитай да намериш в интернет…") has its
// first round call web_search (forcedFirstTool): the model was asked to look, not to choose.

// lookupPlace is where an answer says it looked: one of the lookup* constants, "" for nowhere.
type lookupPlace string

const (
	lookupLibrary lookupPlace = "library" // the books: search_documents, list_documents
	lookupWeb     lookupPlace = "web"     // the internet: web_search, web_fetch
	lookupFolder  lookupPlace = "folder"  // Lili's folder of files: the group of the files
	lookupAdd     lookupPlace = "add"     // into the library: add_to_library
)

const (
	// lookupNote is added to the message of the user, for the model only, when the turn is asked
	// again; %s is the place.
	lookupNote = "[Note for the assistant, not written by the user: your first answer to this message said that you had looked, or would look, in %s, " +
		"but no tool was called and nothing was looked at. Look now with the tool, then answer from what it returned; " +
		"if it found nothing that fits, say so plainly. Do not repeat the first answer.]"

	// lookupCorrection goes under an answer that says it looked when no tool was called and the
	// turn could not be asked again (the tools of that place are not on offer).
	lookupCorrection = "⚠️ Поправка: в този отговор не съм търсила никъде — не е извикан нито един инструмент; написаното е от общите ми знания."

	// maxLookupClaimRunes: the claim opens the answer ("Търсих в…", "Намерих няколко източника…"
	// in all five real answers); a word deep in a long answer is more likely about something else.
	maxLookupClaimRunes = 800
)

// lookupPlaces are what differs between the places: the mode whose tools the retry requires, the
// words for the model and the line the client sees before the second answer.
var lookupPlaces = map[lookupPlace]struct {
	mode, forModel, notice string
}{
	lookupLibrary: {ModeBooks, "the library of the user's books (search_documents)", "\n\n⚠️ Всъщност не бях търсила в библиотеката — търся сега.\n\n"},
	lookupWeb:     {ModeWeb, "the internet (web_search)", "\n\n⚠️ Всъщност не бях търсила в интернет — търся сега.\n\n"},
	lookupFolder:  {ModeFiles, "your folder of files (list_files, read_file)", "\n\n⚠️ Всъщност не бях проверила папката с файловете — проверявам сега.\n\n"},
	lookupAdd:     {ModeLibrary, "", "\n\n⚠️ Всъщност не бях добавила нищо в библиотеката — добавям сега.\n\n"},
}

// addCorrection goes under an answer that says it put something into the library when nothing was
// (add_to_library was not on offer: no link the person wrote).
const addCorrection = "⚠️ Поправка: в този отговор не съм добавила нищо в библиотеката — не е извикан нито един инструмент. За да добавя документ, прати ми линка към него."

// addNote is the note of the retry of an answer that said it put the links into the library.
const addNote = "[Note for the assistant, not written by the user: your first answer to this message said that you had put the links into the library, " +
	"or would put them there now, but add_to_library was not called and nothing was added. Call add_to_library now with the links the user wrote, " +
	"then tell the user plainly what it returned. Do not repeat the first answer.]"

var (
	// lookupSearched: "търсих", "проверих", "прегледах" - the assistant says it looked. With "не"
	// before it the answer is honest ("Не търсих в библиотеката") and is not a claim.
	lookupSearched = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(не\s+)?(?:търсих|потърсих|претърсих|проверих|прегледах|разгледах|прерових|i\s+(?:have\s+)?(?:searched|checked|looked|went\s+through))(?:[^\p{L}]|$)`)
	// lookupFound: "намерих", "открих", "не откривам" - a finding, or its lack, is a claim of a look.
	lookupFound = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:намерих|открих|не\s+(?:откривам|намирам|открих|намерих)|i\s+(?:have\s+)?found|i\s+(?:could\s+not|couldn['’]?t|did\s+not|didn['’]?t)\s+find)(?:[^\p{L}]|$)`)
	// lookupWill: "ще потърся", "нека проверя" - a look promised now; a condition ("ако искаш, ще
	// потърся") is an offer and is not one (see lookupClaim).
	lookupWill = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:(?:ще|нека)\s+(?:потърся|проверя|търся|прегледам|претърся|разгледам)|i['’]ll\s+(?:search|look|check)|let\s+me\s+(?:search|look|check))(?:[^\p{L}]|$)`)
	// lookupCondition: the sentence is an offer or a condition - "ако искаш, ще потърся", "веднага
	// след това ще проверя мащаба им" (after the person sends the links; 2026-11-20, 17:57).
	lookupCondition = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:ако|щом|когато|след\s+това|след\s+като|if|once|when|after|искаш\s+ли|would\s+you)(?:[^\p{L}]|$)`)

	// The places, by the words of the sentence. "файл" alone is not one: the person calls the
	// books "файловете", and Lili her own notes.
	lookupLibraryWords = regexp.MustCompile(`(?i)(?:библиотек|книг|(?:^|[^\p{L}])(?:library|books?)(?:[^\p{L}]|$))`)
	lookupFolderWords  = regexp.MustCompile(`(?i)(?:папк|(?:^|[^\p{L}])folder)`)
	lookupWebWords     = regexp.MustCompile(`(?i)(?:интернет|онлайн|(?:^|[^\p{L}])(?:уеба?|мрежата|нета|google|гугъл|internet|online|web)(?:[^\p{L}]|$))`)
	// lookupSourceWords: "намерих няколко източника" is a look on the internet only after a
	// message that asked to look (2026-11-24, 21:30); after "какво виждаш в ревюто" the sources
	// are those of the person's own text.
	lookupSourceWords = regexp.MustCompile(`(?i)(?:източни|сайт|(?:^|[^\p{L}])(?:sources?|websites?|sites?)(?:[^\p{L}]|$))`)

	// sentenceEnd splits an answer into sentences, roughly.
	sentenceEnd = regexp.MustCompile(`[.!?\n]+`)

	// addDone and addWill: the assistant says it put something into the library, or will now
	// ("добавих ги", "ще ги добавя веднага"); with a word for the library in the same sentence.
	addDone = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:добавих|свалих|качих|прибрах|i\s+(?:have\s+)?(?:added|saved|put))(?:[^\p{L}]|$)`)
	addWill = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:(?:ще|нека)\s+(?:\p{L}+\s+){0,2}?(?:добавя|сваля|кача|прибера)|i['’]ll\s+add|let\s+me\s+add)(?:[^\p{L}]|$)`)
	// addNow: a promise said to be kept at once is one even in an answer that ends with a question.
	addNow = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:веднага|сега|now|right\s+away)(?:[^\p{L}]|$)`)

	// webAskVerb and webAskPlace: a message that asks to look something up on the internet.
	// The forms of the verbs are listed: a stem would take "виждаш" for "виж" and "проверка" for
	// "провери".
	webAskVerb = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:` +
		`намери|намериш|намерите|намерете|потърси|потърсиш|потърсите|потърсете|търси|търсиш|търсете|търсене|търсенето|` +
		`провери|провериш|проверете|открий|откриеш|виж|вижте|прегледай|прегледаш|` +
		`search|find|look\s+up|google` +
		`)(?:[^\p{L}]|$)`)
	webAskPlace = regexp.MustCompile(`(?i)(?:(?:в|из|от)\s+(?:интернет|мрежата|нета|google|гугъл|уеба)|онлайн|the\s+(?:internet|web)|online|on\s+google)`)
)

// lookupClaim returns where an answer to message says it looked, or will look now, or "" when
// it says neither. Meant for an answer after which no tool was called. A plain filter: a false
// alarm costs one more request to the model, which then looks.
func lookupClaim(answer, message string) lookupPlace {
	answer = strings.TrimSpace(answer)
	offer := strings.HasSuffix(answer, "?")
	if r := []rune(answer); len(r) > maxLookupClaimRunes {
		answer = string(r[:maxLookupClaimRunes])
	}
	askedToLook := webAskVerb.MatchString(message) || lookupWebWords.MatchString(message)
	for _, sentence := range sentenceEnd.Split(answer, -1) {
		if lookupLibraryWords.MatchString(sentence) && claimsAnAdd(sentence, offer) {
			return lookupAdd
		}
		if !claimsALook(sentence, offer) {
			continue
		}
		switch {
		case lookupLibraryWords.MatchString(sentence):
			return lookupLibrary
		case lookupFolderWords.MatchString(sentence):
			return lookupFolder
		case lookupWebWords.MatchString(sentence), askedToLook && lookupSourceWords.MatchString(sentence):
			return lookupWeb
		}
	}
	return ""
}

// claimsALook tells whether a sentence says the assistant looked, or will look now. offer: the
// answer ends with a question, so a promise in it hands the turn back to the user.
func claimsALook(sentence string, offer bool) bool {
	for _, m := range lookupSearched.FindAllStringSubmatch(sentence, -1) {
		if m[1] == "" {
			return true
		}
	}
	if lookupFound.MatchString(sentence) {
		return true
	}
	return !offer && lookupWill.MatchString(sentence) && !lookupCondition.MatchString(sentence)
}

// claimsAnAdd tells whether a sentence says the assistant put something into the library, or will now.
func claimsAnAdd(sentence string, offer bool) bool {
	if lookupCondition.MatchString(sentence) {
		return false
	}
	if addDone.MatchString(sentence) {
		return true
	}
	return addWill.MatchString(sentence) && (!offer || addNow.MatchString(sentence))
}

// asksToSearchWeb tells whether a message asks to look something up on the internet: "опитай да
// намериш в интернет свободна статия…".
func asksToSearchWeb(message string) bool {
	message = strings.TrimSpace(message)
	if len([]rune(message)) > maxKnowledgeQuestionRunes {
		return false
	}
	return webAskVerb.MatchString(message) && webAskPlace.MatchString(message)
}

// retryUnbackedLookup asks the turn once more when its answer says it looked somewhere and no
// tool was called (see the top of this file): the tools of that place are offered and the first
// round must call one. The client has seen the first answer: a line under it says it did not look
// and the second answer follows. What is returned, and stored, is the second answer. Where the
// turn called a tool, the answer says nothing that needs this, or the tools are not on offer, the
// answer is returned as it is.
func (o *Orchestrator) retryUnbackedLookup(
	ctx context.Context,
	messages []openai.ChatCompletionMessage,
	tools []openai.Tool,
	content string,
	executed []openai.ToolCall,
	onDelta deltaFunc,
) (string, []openai.ToolCall, error) {
	if len(executed) > 0 {
		return content, executed, nil
	}
	place := lookupClaim(content, lastUserContent(messages))
	if place == "" {
		return content, executed, nil
	}
	how := lookupPlaces[place]
	open, required := o.modeTools(tools, how.mode, turnFrom(ctx))
	if len(required) == 0 {
		return content, executed, nil
	}
	slog.WarnContext(ctx, "the answer says it looked somewhere and no tool was called; the turn is asked again with the tools of that place",
		"place", string(place))
	metrics.UserFileOps.WithLabelValues("lookup_"+string(place), "retried").Inc()
	if onDelta != nil {
		if err := onDelta(how.notice, ""); err != nil {
			return content, executed, err
		}
	}
	note := fmt.Sprintf(lookupNote, how.forModel)
	if place == lookupAdd {
		note = addNote
	}
	return o.runRounds(ctx, withNoteOnLastUser(messages, note), open, onDelta, required)
}
