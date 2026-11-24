package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/api"
	"go-monolith/internal/reminders"
	"go-monolith/metrics"
)

// Reminders (plan item И8, 2026-11-24): "напомни ми утре в 9 да платя тока", "всяка сутрин в 8…",
// "рожденият ден на мама е на 12 март", "какви напомняния имам?", "махни напомнянето за тока".
//
// The code answers all of it. Only the reading of a request is the model's, in a call of its own
// (reminders.Extractor): the code checks the day and the hour against what it reads in the words,
// says them back with the day of the week and the year, and keeps the reminder after "да". The
// sender (reminders.Runner) writes it into this conversation and sends it to Telegram in its hour.
//
// Before this, asked to remind, the chat model said "Ще ти напомням за тях, когато времето стане
// подходящо" (2026-11-24, 18:41) - an ability it did not have. Off unless REMINDERS_ENABLED.

// ModeReminder is the button of the reminders: the message is a request for one, whatever it says.
const ModeReminder = "reminder"

const (
	// reminderMaxRunes: a request for a reminder is a sentence or two, not a story.
	reminderMaxRunes = 400
	// correctionMaxRunes: "не, в 10", "по-добре в петък в 18:00".
	correctionMaxRunes = 120
	// reminderListed is the most reminders the list shows.
	reminderListed = 30
)

var (
	// reminderWords: "напомни", "напомняй", "напомняне", "remind".
	reminderWords = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:напомн\p{L}*|remind\p{L}*)`)
	// reminderVerbs: a request to be reminded ("напомни ми", "напомняй ми", "ще ми напомниш ли", "remind me").
	reminderVerbs = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:напомни|напомниш|напомните|напомнете|напомняй|напомняйте|напомняш|remind)(?:[^\p{L}]|$)`)
	// recallWords: "напомни ми какво говорихме" asks for the conversation, not for a reminder.
	recallWords = regexp.MustCompile(`(?i)напомн\p{L}*\s+ми\s+(?:отново\s+)?(?:какво|кой|коя|кое|кои|как|къде|защо|колко|за\s+какво\s+(?:говорихме|си\s+говорихме|ставаше))(?:[^\p{L}]|$)`)
	// eventWords: a date of every year ("рожденият ден на мама е на 12 март").
	eventWords = regexp.MustCompile(`(?i)(?:рожден\p{L}*\s+ден|имен\p{L}*\s+ден|годишнин\p{L}*|birthday|anniversary)`)
	// reminderListWords: "какви напомняния имам?", "покажи напомнянията", "/reminders".
	reminderListWords = regexp.MustCompile(`(?i)(?:(?:какви|кои|колко|покажи|изреди|списък|списъка|имам\s+ли)\p{L}*\s+(?:\p{L}+\s+){0,2}напомнян\p{L}*|напомнян\p{L}*\s+(?:\p{L}+\s+){0,1}(?:имам|са\s+записани|си\s+записала)|^\s*/?reminders\s*$)`)
	// reminderRemoveWords: "махни напомнянето за тока", "изтрий 2".
	reminderRemoveWords = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:махни|махнете|изтрий|изтрийте|премахни|отмени|откажи|спри|изчисти|delete|remove|cancel)(?:[^\p{L}]|$)`)
	// correctionOpening: "не, …", "по-добре …", "всъщност …", "а в 10?".
	correctionOpening = regexp.MustCompile(`(?i)^\s*(?:не|по-добре|всъщност|поправи|смени|промени|а|нека|направи\s+го|no|actually|make\s+it)(?:[^\p{L}]|$)`)
	// listNumber: the number in "махни 2", "изтрий второто" is read as a digit only.
	listNumber = regexp.MustCompile(`(?:^|[^\d])(\d{1,2})(?:[^\d]|$)`)
	// reminderStopWords are the words of a request to remove that do not name a reminder.
	reminderStopWords = map[string]bool{"махни": true, "изтрий": true, "премахни": true, "отмени": true, "откажи": true, "спри": true,
		"моля": true, "напомнянето": true, "напомнянията": true, "напомняне": true, "напомняния": true, "искам": true, "повече": true,
		"трябва": true, "нужно": true, "това": true, "онова": true, "всички": true}
)

// reminderDesk is what the chat needs of the reminders.
type reminderDesk struct {
	store     *reminders.Store
	extractor *reminders.Extractor
	wake      func()
	now       func() time.Time
	loc       *time.Location
}

// WithReminders turns the reminders on: store keeps them, extractor reads a request, wake tells the
// sender that one was kept.
func (o *Orchestrator) WithReminders(store *reminders.Store, extractor *reminders.Extractor, wake func()) *Orchestrator {
	o.reminders = &reminderDesk{store: store, extractor: extractor, wake: wake, now: time.Now, loc: reminders.Location()}
	return o
}

// asksForReminder tells whether a message asks to be reminded of something: a word for it (and
// not "напомни ми какво говорихме"), or the date of an event of every year.
func asksForReminder(message string) bool {
	message = strings.TrimSpace(withoutClosingNote(message))
	if message == "" || utf8.RuneCountInString(message) > reminderMaxRunes || pageLink.MatchString(message) {
		return false
	}
	if recallWords.MatchString(message) || asksForReminderList(message) || asksToRemoveReminder(message) {
		return false
	}
	if reminderVerbs.MatchString(message) {
		return true
	}
	// "напомняне" or a birthday asks for a reminder only with a day or an hour, and not in a
	// request about a file ("запиши в бележки.md: напомняне за тока").
	if (reminderWords.MatchString(message) || eventWords.MatchString(message)) && !fileMention.MatchString(message) {
		r := reminders.Read(message, time.Now(), reminders.Location())
		return r.Day != nil || r.FromNow > 0 || (r.Minute != nil && reminderWords.MatchString(message))
	}
	return false
}

func asksForReminderList(message string) bool {
	m := strings.TrimSpace(message)
	return utf8.RuneCountInString(m) <= correctionMaxRunes && reminderListWords.MatchString(m) && !reminderRemoveWords.MatchString(m)
}

func asksToRemoveReminder(message string) bool {
	m := strings.TrimSpace(message)
	return utf8.RuneCountInString(m) <= correctionMaxRunes && reminderRemoveWords.MatchString(m) && reminderWords.MatchString(m)
}

// reminderAnswer is the answer of the code to a message about reminders, "" when the model is to
// answer it. forced: the message came with the button of the reminders.
func (o *Orchestrator) reminderAnswer(ctx context.Context, userID uuid.UUID, session, message string, history []openai.ChatCompletionMessage, forced bool) string {
	desk := o.reminders
	if desk == nil {
		return ""
	}
	message = strings.TrimSpace(withoutClosingNote(message))
	now := desk.now()

	proposal, err := desk.store.Proposed(ctx, userID, session)
	if err != nil {
		slog.WarnContext(ctx, "could not read the proposed reminder; the model answers", "err", err)
		return ""
	}
	button := forced
	var earlier []reminders.Turn
	if proposal != nil {
		reading := reminders.Read(message, now, desk.loc)
		readSomething := reading.Day != nil || reading.Minute != nil || reading.FromNow > 0
		switch {
		case isYes(message) && !readSomething:
			return desk.keep(ctx, userID, session, *proposal, now)
		case isNo(message) && !readSomething && utf8.RuneCountInString(message) <= 15:
			_ = desk.store.DropProposed(ctx, userID, session)
			metrics.Reminders.WithLabelValues("declined").Inc()
			return "Добре, няма да го записвам."
		case utf8.RuneCountInString(message) <= correctionMaxRunes && (readSomething || correctionOpening.MatchString(message) || forced):
			// A correction of the proposal: the model reads it with the proposal before it.
			d := reminders.Draft{Text: proposal.Text, Rule: proposal.Rule, Next: proposal.Next, Early: proposal.Early}
			earlier = []reminders.Turn{{Role: "assistant", Content: reminders.ProposalText(d, now, desk.loc, false)}}
			forced = true
		default:
			// Something else: the proposal is not left to be kept by a later "да".
			_ = desk.store.DropProposed(ctx, userID, session)
			metrics.Reminders.WithLabelValues("declined").Inc()
		}
	}

	if earlier == nil {
		switch {
		case asksForReminderList(message):
			return desk.list(ctx, userID, now)
		case asksToRemoveReminder(message) || removesFromList(history, message):
			return desk.remove(ctx, userID, message, now)
		}
	}
	if !forced && !asksForReminder(message) {
		return ""
	}

	fields, err := desk.extractor.Extract(ctx, message, earlier, now, desk.loc)
	if err != nil {
		metrics.Reminders.WithLabelValues("extract_failed").Inc()
		slog.WarnContext(ctx, "the request for a reminder could not be read", "err", err)
		return "Не успях да разчета напомнянето — моделът не отговори както трябва. Опитай пак след малко " +
			"(например „напомни ми утре в 9 да платя тока“). Нищо не е записано."
	}
	draft, problem := reminders.Build(fields, message, now, desk.loc)
	if problem == reminders.NotReminder && !button {
		// "напомни ми …" that asks for something else, or a message after a proposal that is no
		// correction of it ("а какво е времето?"): the model answers, and the proposal is dropped.
		if earlier != nil {
			_ = desk.store.DropProposed(ctx, userID, session)
		}
		metrics.Reminders.WithLabelValues("problem_" + problem).Inc()
		return ""
	}
	if problem != "" {
		metrics.Reminders.WithLabelValues("problem_" + problem).Inc()
		return reminders.ProblemText(problem, draft, desk.loc)
	}
	for _, c := range draft.Corrected {
		metrics.Reminders.WithLabelValues("corrected_" + c).Inc()
	}
	if len(draft.Corrected) > 0 {
		slog.InfoContext(ctx, "the code corrected the reminder the model read", "corrected", draft.Corrected,
			"model_date", fields.Date, "model_time", fields.Time, "model_minutes_from_now", fields.MinutesFromNow)
	}
	if _, err := desk.store.Propose(ctx, userID, session, draft); err != nil {
		slog.WarnContext(ctx, "the proposed reminder could not be stored", "err", err)
		return "Не успях да запазя напомнянето (грешка в базата). Опитай пак след малко. Нищо не е записано."
	}
	metrics.Reminders.WithLabelValues("proposed").Inc()
	return reminders.ProposalText(draft, now, desk.loc, desk.hasTelegram(ctx, userID, session))
}

// keep makes the proposal a reminder, after the person's yes.
func (d *reminderDesk) keep(ctx context.Context, userID uuid.UUID, session string, p reminders.Reminder, now time.Time) string {
	kept, ok, err := d.store.Activate(ctx, userID, p, now, d.loc)
	if errors.Is(err, reminders.ErrGone) {
		return "Това напомняне вече е записано (или отказано). Кажи „какви напомняния имам?“, за да ги видиш."
	}
	if err != nil {
		slog.WarnContext(ctx, "the reminder could not be kept", "err", err)
		return "Не успях да запиша напомнянето (грешка в базата). Опитай пак след малко."
	}
	if !ok {
		return "Докато чаках „да“, времето на напомнянето мина, затова не го записах. Кажи ми го пак с новото време."
	}
	metrics.Reminders.WithLabelValues("kept").Inc()
	if d.wake != nil {
		d.wake()
	}
	where := "тук, в разговора"
	if d.hasTelegram(ctx, userID, session) {
		where = "в Telegram и тук, в разговора"
	}
	return fmt.Sprintf("Записах ✅ Ще ти напомня „%s“ — %s. Ще дойде %s.\n"+
		"Ако компютърът е изключен в този час, напомнянето идва при следващото пускане, със „закъсняло“. "+
		"„Какви напомняния имам?“ ги показва, „махни …“ маха едно.",
		kept.Text, reminders.WhenText(kept.Rule, kept.Next, now, d.loc), where)
}

// hasTelegram tells whether a reminder of the conversation reaches the person's Telegram too.
func (d *reminderDesk) hasTelegram(ctx context.Context, userID uuid.UUID, session string) bool {
	if api.IsTelegramSession(session) {
		return true
	}
	_, ok, err := d.store.TelegramChat(ctx, userID)
	return err == nil && ok
}

// list is the answer to "какви напомняния имам?".
func (d *reminderDesk) list(ctx context.Context, userID uuid.UUID, now time.Time) string {
	active, err := d.store.Active(ctx, userID)
	if err != nil {
		slog.WarnContext(ctx, "could not list the reminders", "err", err)
		return "Не успях да прочета напомнянията (грешка в базата). Опитай пак след малко."
	}
	metrics.Reminders.WithLabelValues("listed").Inc()
	if len(active) == 0 {
		return "Нямаш записани напомняния. Кажи ми например „напомни ми утре в 9 да платя тока“ или „всеки понеделник в 8 да проверя пощата“."
	}
	return listText(active, now, d.loc)
}

// reminderListHead begins the list, so that "махни 2" right after it is read against it.
const reminderListHead = "Напомнянията ти"

func listText(active []reminders.Reminder, now time.Time, loc *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%d):\n", reminderListHead, len(active))
	for i, r := range active {
		if i == reminderListed {
			fmt.Fprintf(&b, "… и още %d\n", len(active)-reminderListed)
			break
		}
		when := reminders.AtText(r.Next, now, loc)
		if rep := reminders.RepeatText(r.Rule); rep != "" {
			when = rep + "; следващото — " + when
		}
		fmt.Fprintf(&b, "%d. „%s“ — %s\n", i+1, r.Text, when)
	}
	b.WriteString("\nЗа да махнеш едно: „махни 2“ или „махни напомнянето за …“.")
	return b.String()
}

// removesFromList tells whether a short "махни 2" answers the list of the reminders that the code
// gave last in this conversation.
func removesFromList(history []openai.ChatCompletionMessage, message string) bool {
	if utf8.RuneCountInString(message) > 30 || !reminderRemoveWords.MatchString(message) || !listNumber.MatchString(message) {
		return false
	}
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == openai.ChatMessageRoleAssistant {
			return strings.HasPrefix(strings.TrimSpace(history[i].Content), reminderListHead)
		}
	}
	return false
}

// remove is the answer to "махни напомнянето за тока" and "махни 2".
func (d *reminderDesk) remove(ctx context.Context, userID uuid.UUID, message string, now time.Time) string {
	active, err := d.store.Active(ctx, userID)
	if err != nil {
		slog.WarnContext(ctx, "could not list the reminders", "err", err)
		return "Не успях да прочета напомнянията (грешка в базата). Опитай пак след малко."
	}
	if len(active) == 0 {
		return "Нямаш записани напомняния, няма какво да махна."
	}
	var pick []reminders.Reminder
	if m := listNumber.FindStringSubmatch(message); m != nil {
		if n, _ := strconv.Atoi(m[1]); n >= 1 && n <= len(active) {
			pick = []reminders.Reminder{active[n-1]}
		}
	}
	if pick == nil {
		pick = matchingReminders(active, message)
	}
	switch len(pick) {
	case 0:
		return "Не намерих такова напомняне. " + listText(active, now, d.loc)
	case 1:
		ok, err := d.store.Cancel(ctx, userID, pick[0].ID)
		if err != nil || !ok {
			slog.WarnContext(ctx, "could not cancel the reminder", "err", err, "found", ok)
			return "Не успях да махна напомнянето. Опитай пак след малко."
		}
		metrics.Reminders.WithLabelValues("cancelled").Inc()
		return fmt.Sprintf("Махнах напомнянето „%s“ ✅", pick[0].Text)
	}
	var b strings.Builder
	b.WriteString("Кое от тези да махна? Кажи „махни“ и номера му:\n")
	for _, r := range pick {
		for i, a := range active {
			if a.ID == r.ID {
				fmt.Fprintf(&b, "%d. „%s“ — %s\n", i+1, r.Text, reminders.AtText(r.Next, now, d.loc))
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// matchingReminders are the reminders that share the most words with the message (by the first
// five letters of a word, so that "тока" finds "тока" and "срещата" finds "среща").
func matchingReminders(active []reminders.Reminder, message string) []reminders.Reminder {
	stems := func(s string) map[string]bool {
		out := map[string]bool{}
		for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !(r >= 'а' && r <= 'я' || r >= 'a' && r <= 'z' || r == 'ѝ') }) {
			if utf8.RuneCountInString(w) < 3 || reminderStopWords[w] {
				continue
			}
			rs := []rune(w)
			out[string(rs[:min(5, len(rs))])] = true
		}
		return out
	}
	want := stems(message)
	best, bestScore := []reminders.Reminder(nil), 0
	for _, r := range active {
		score := 0
		for s := range stems(r.Text) {
			if want[s] {
				score++
			}
		}
		switch {
		case score == 0:
		case score > bestScore:
			best, bestScore = []reminders.Reminder{r}, score
		case score == bestScore:
			best = append(best, r)
		}
	}
	return best
}
