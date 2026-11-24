package agent

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"go-monolith/metrics"
)

// "Какви инструменти имаш и как да ги извикам?" (the owner, 2026-11-23). The real Gemma, asked
// six times, said in general words that it can search the books and the web and write files and
// that no commands are needed - never a thing it can do with the words that ask for it, nothing
// of what the code does on its own (the whole page into a file, the explanation from the books,
// "успя ли?", a title for the library), and three times of three it searched the books for the
// question first. Much of what it can do is not in front of it: the file tools are behind their
// stub, and the explanation, the status and the saved page are started by the code from the
// words of the message.
//
// So the code answers such a question itself, as it answers "успя ли?" (status_answer.go): a
// short list with a sentence to write for each thing, and only the things that are on now - the
// list follows the switches of the .env. No model is asked. Off unless HELP_ANSWER_ENABLED.

// helpMaxRunes: a longer message asks for something more than the list.
const helpMaxRunes = 120

var (
	helpQuestion = regexp.MustCompile(`(?i)(какви\s+(са\s+)?(ти\s+)?(инструменти|възможности|функции|умения|команди)|` +
		`какво\s+(още\s+)?(можеш|умееш)(\s+да\s+(правиш|вършиш))?\s*[?!.]*\s*$|` +
		`как\s+да\s+те\s+(извикам|викам|използвам|ползвам)|` +
		`(напомни|покажи|кажи)\s+ми\s+какво\s+(можеш|умееш)|` +
		`с\s+какво\s+(можеш\s+да\s+)?(ми\s+)?помогнеш\s*[?!.]*\s*$|` +
		`^\s*/?(помощ|help|команди)\s*[?!.]*\s*$|` +
		`what\s+(can\s+you\s+do|tools\s+do\s+you\s+have))`)
	// helpNot: a link, or a request that names what to do, is a request, not a question about the list.
	helpNot = regexp.MustCompile(`(?i)(https?://|можеш\s+ли\s+да|можеш\s+да\s+ми\s+(кажеш|обясниш|направиш|намериш|потърсиш))`)
)

// asksForHelp tells whether a message asks what the assistant can do and how to ask for it.
func asksForHelp(message string) bool {
	message = strings.TrimSpace(withoutClosingNote(message))
	if message == "" || utf8.RuneCountInString(message) > helpMaxRunes || helpNot.MatchString(message) {
		return false
	}
	return helpQuestion.MatchString(message)
}

// WithHelpAnswer turns on the answer of the code to a question about what the assistant can do.
func (o *Orchestrator) WithHelpAnswer() *Orchestrator {
	o.helpAnswer = true
	return o
}

// helpAnswerFor is the answer of the code to the message, "" when the model answers it.
func (o *Orchestrator) helpAnswerFor(message string) string {
	if !o.helpAnswer || !asksForHelp(message) {
		return ""
	}
	metrics.HelpAnswers.Inc()
	return o.helpText()
}

// helpText is the list of what is on now, with what to write for each.
func (o *Orchestrator) helpText() string {
	files := slices.ContainsFunc(o.groups, func(g toolGroup) bool { return g.name == filesGroup().name })
	var b strings.Builder
	b.WriteString("Ето какво мога сега и как да ме помолиш (не е нужно дословно — пиши както ти е удобно, това са примери):\n")

	b.WriteString("\n📚 Книгите ти\n")
	b.WriteString("• „Какво казва Сенека за гнева?“, „Намери ми откъс за дисциплината“\n")
	b.WriteString("• „Имаш ли книгата …?“, „Какви книги имаш за …?“\n")
	if o.explainDossier {
		b.WriteString("• „Не разбирам какво е …“, „Обясни ми …“ — обяснявам по книгите ти, с имената им\n")
	}

	b.WriteString("\n🌐 Интернет\n")
	b.WriteString("• „Потърси в интернет …“\n")
	b.WriteString("• „Прочети тази страница: <линк>“ (и PDF)\n")

	if o.addToLibrary {
		b.WriteString("\n🗂 Библиотеката\n")
		b.WriteString("• „Добави в библиотеката: <линк>“ — свалям документа и го добавям\n")
		b.WriteString("• „… под заглавие „…“: <линк>“ — с името, което искаш\n")
		if o.runPlans != nil && files {
			b.WriteString("• „За всеки от добавените източници направи файл с ключовите му факти“ (или „с резюме“) — " +
				"показвам списъка, ти казваш „да“, и работя на заден план; „докъде стигна?“, „спри“\n")
			b.WriteString("• „За всеки от добавените източници извлечи всичко подробно в отделен файл“ — чета всеки документ целия, на части, и бележките растат във файла; за един: „прочети цялата статия и запиши подробни бележки във файл“ (последният добавен)\n")
		}
	}

	if files {
		b.WriteString("\n📝 Моите файлове (табът „Файлове на Лили“)\n")
		b.WriteString("• „Запиши във файл бележки.md: …“, „Допиши в бележки.md: …“\n")
		b.WriteString("• „Отвори бележки.md“, „Какви файлове имаш?“\n")
		b.WriteString("• „Запиши цялата статия във файл: <линк>“ — целият текст, дословно; потвърждаваш го ти\n")
		if o.statusAnswer {
			b.WriteString("• „Успя ли?“ — казвам кои файлове са променени и кога\n")
		}
		b.WriteString("• Изтриване и „Запази в библиотеката“ — с копчетата до файла\n")
	}

	b.WriteString("\n🧠 Паметта\n")
	b.WriteString("• „Запомни, че …“, „Какво помниш за мен?“\n")

	if o.reminders != nil {
		b.WriteString("\n⏰ Напомняния (идват в Telegram и тук)\n")
		b.WriteString("• „Напомни ми утре в 9 да платя тока“, „… след 20 минути …“, „… в петък в 18:30 …“ — казвам датата обратно, записвам след „да“\n")
		b.WriteString("• „Напомняй ми всяка сутрин в 8 …“, „… всеки понеделник …“, „… всеки месец на 5-ти …“\n")
		b.WriteString("• „Рожденият ден на мама е на 12 март“ — всяка година, ден преди това и в самия ден\n")
		b.WriteString("• „Какви напомняния имам?“, „Махни напомнянето за тока“\n")
	}

	b.WriteString("\n⚙️ Как да говорим\n")
	b.WriteString("• „Отсега нататък винаги …“ — предлагам правило, ти го потвърждаваш\n")

	if o.attachments != nil {
		b.WriteString("\n📎 Прикачи файл в чата и питай за него — „Какво има в прикачения файл?“, „Провери кода за грешки“\n")
	}
	buttons, commands := "Бележките на Лили, Памет, Как да говорим", "/books, /web, /add, /notes, /memory, /style"
	if o.reminders != nil {
		buttons, commands = buttons+", Напомняне", commands+", /remind (и /reminders — списъкът)"
	}
	b.WriteString("\n🎛 Ако не помниш как се казва: менюто до полето за писане във Flet (бутоните Книги, Интернет, Библиотека, " +
		buttons + ") праща следващото съобщение точно по този път; в Telegram — " +
		commands + " и текстът след командата\n")
	if o.paid != nil {
		b.WriteString("\n☁️ Копчето облак / вкъщи на разговора във Flet решава кой модел отговаря\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
