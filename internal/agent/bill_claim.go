package agent

import (
	"regexp"
	"strings"
)

// The model says it saved a bill or set a reminder; it has no tool for either. In the Flet chat of
// 2026-11-25 (16:52-17:06) it answered "добавям датата 15.09.2027…", "актуализирам сумата на 87,00
// евро…", "ще те напомня точно една седмица преди…", "Записах в базата, че наемът…" - and no bill
// and no reminder were kept: the code keeps them, after the "да" to a proposal of its own
// (bill_answer.go, reminder_answer.go). The day before it promised "Ще ти напомням за тях" with no
// reminders at all. Such an answer gets a line under it that says so and how to do it.

const billClaimCorrection = "⚠️ Поправка: в този отговор нищо не е записано — нито сметка, нито напомняне. Тях ги записва кодът след „да“ на " +
	"неговото предложение: за напомняне напиши „напомни ми на … да …“, за сметка — качи я пак или „запиши, че всеки месец на … плащам … € на …“."

var (
	// billClaimWords: a bill or a reminder said to be saved, changed or to come.
	billClaimWords = regexp.MustCompile(`(?i)(?:` +
		`(?:записах|запазих|съхраних)\s+(?:\p{L}+\s+){0,6}?в\s+(?:базата|системата|сметките)|` +
		`ще\s+(?:(?:ти|те|ви)\s+)?напомн|напомнянето\s+ще|ще\s+получиш\s+(?:напомн|известие)|ще\s+те\s+предупредя|` +
		`(?:добавям|добавих|актуализирам|актуализирах|променям|промених|обнових|обновявам)\s+(?:\p{L}+\s+){0,3}?(?:срок|сума|дата)\p{L}*` +
		`)`)
	// billClaimDone / billClaimObject: in a turn that called no tool, "Добавих наема… Вече е включен в
	// списъка ти", "Готово, промених статуса му" (2026-11-25, 17:37-17:40): a change of the bills
	// said to be done. With a tool called the same words may be true of a file.
	billClaimDone   = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:добавих|включих|промених|записах|запазих|обнових|отбелязах|маркирах|отразих)(?:[^\p{L}]|$)`)
	billClaimObject = regexp.MustCompile(`(?i)(?:сметк|наем|плащан|разход|списъ|статус|баз[аи])`)
	// billClaimOffer: the sentence offers or asks ("ако искаш, ще ти напомня").
	billClaimOffer = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:ако|искаш\s+ли|желаеш\s+ли|if)(?:[^\p{L}]|$)`)
)

// claimsABillOrReminder tells whether an answer of the model says a bill or a reminder was kept or
// changed, or that a reminder will come; toolCalled widens nothing, it narrows: the words of a done
// change count only in a turn without a tool. A plain filter: a false alarm is a line under the answer.
func claimsABillOrReminder(answer string, toolCalled bool) bool {
	for _, sentence := range sentenceEnd.Split(strings.TrimSpace(answer), -1) {
		if billClaimOffer.MatchString(sentence) {
			continue
		}
		if billClaimWords.MatchString(sentence) {
			return true
		}
		if !toolCalled && billClaimDone.MatchString(sentence) && billClaimObject.MatchString(sentence) {
			return true
		}
	}
	return false
}
