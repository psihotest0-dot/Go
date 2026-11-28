package agent

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// A task with many items is not left to the model's choice to plan.
//
// On 2026-11-21 the real model was given "сега за всеки от добавените източници направи отделен
// файл с ключовите му факти" with the plan on offer (a stub with examples, as the file tools
// have it): it did not call the stub, in either of two replays of the same seven turns, and
// answered "не мога да създам отделни файлове" after three searches; the "продължи" that
// followed got three replies of "сега преминавам към…" with no call. A prompt cannot make it
// plan, as it could not make it ask a question every fourth reply (P49) or search for a passage
// (P11); so, like those, the first round of such a message is required to call the stub.
//
// What is asked for here is only the plan. The tools of the plan then come on offer and the
// round after must use one (the rule of every group), so the model makes the plan; and from then
// on the plan is in front of it in every turn of the conversation (taskplan.Render).

var (
	// eachItem: "за всеки", "всички", "всяка една", "each", "every": work on every item of a
	// set. "за всеки случай" is "just in case" and is taken out first.
	eachItem   = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:всеки|всяка|всяко|всички|всичките|поотделно|един\s+по\s+един|each|every|one\s+by\s+one|separately)(?:[^\p{L}]|$)`)
	// So is a time that repeats ("всеки ден", "всяка сутрин"): "запиши във файл навици.md: … всеки
	// ден …" was taken for work on every item and made a plan (the live check of 2026-11-28).
	justInCase = regexp.MustCompile(`(?i)за\s+всеки\s+случай|в\s+всеки\s+случай|във\s+всеки\s+случай|при\s+всички\s+случаи|на\s+всяка\s+цена|in\s+any\s+case|just\s+in\s+case|` +
		`(?:^|[^\p{L}])(?:всеки|всяка|всяко|всички|every|each)\s+(?:ден|дни|дена|път|пъти|месец|месеца|седмица|седмици|година|години|час|часа|минута|минути|сутрин|вечер|нощ|утро|понеделник|вторник|сряда|четвъртък|петък|събота|неделя|day|days|week|weeks|month|months|year|years|hour|hours|morning|evening|night|time|times)(?:[^\p{L}]|$)`)
	// itemNoun: what there are many of: the sources, files, papers, books, links, pages.
	itemNoun = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:` +
		`източни[кц]\p{L}*|файл\p{L}*|документ\p{L}*|книг\p{L}*|книж\p{L}*|статии|статия|статиите|линк\p{L}*|раздел\p{L}*|глав[аи]\p{L}*|страниц\p{L}*|изследван\p{L}*|доклад\p{L}*|` +
		`sources?|files?|documents?|papers?|books?|articles?|links?|chapters?|sections?|pages?|studies|reports?` +
		`)(?:[^\p{L}]|$)`)
	// workVerb: what is to be done to each: make, write, read, take out, process, compare.
	workVerb = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:` +
		`направ|създа|запиш|запаз|напиш|изтегл|извлеч|обработ|добав|прегле|прочет|анализир|сравн|обобщ|резюмир|подготв|изготв|оформ|` +
		`make|create|write|save|read|extract|process|summari[sz]e|compare|analy[sz]e|prepare|download|review` +
		`)\p{L}*`)
)

// maxMultiStepRunes: a long message is a story or the material itself (the links), and the tool
// that does the work stands on its own there.
const maxMultiStepRunes = 400

// asksForManySteps tells whether a message asks for work on every item of a set: "за всеки от
// добавените източници направи отделен файл", "прочети всички статии и ги обобщи", "make a file
// for each paper". A plain filter: a false alarm makes a plan of one or two steps.
func asksForManySteps(message string) bool {
	message = strings.TrimSpace(message)
	if message == "" || utf8.RuneCountInString(message) > maxMultiStepRunes {
		return false
	}
	if justInCase.MatchString(message) {
		message = justInCase.ReplaceAllString(message, " ")
	}
	return eachItem.MatchString(message) && itemNoun.MatchString(message) && workVerb.MatchString(message)
}
