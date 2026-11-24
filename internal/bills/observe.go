package bills

import (
	"fmt"
	"sort"
	"strings"
)

// The observation of "the accountant" is the code's, like the numbers: the largest kind and its
// share, or the kind that changed the most. A comment of the local model was measured first
// (2026-11-24, 12 answers): with thinking on it was always empty; with it off 9 of 12 kept every
// number to the answer's, but one of the 9 said a thing that is not so ("токът е най-голямото
// перо" when the rent was) - a check of the numbers does not catch a wrong fact.

// biggestKindNote is "💡 Най-голямото перо е наем — 610,00 € (80 % от всичко).", "" for one kind.
func biggestKindNote(s Sum) string {
	if len(s.ByKind) < 2 || s.Cents == 0 {
		return ""
	}
	k := s.kindsByAmount()[0]
	return fmt.Sprintf("💡 Най-голямото перо е %s — %s (%d %% от всичко).", KindNames[k], Money(s.ByKind[k], "EUR"), s.ByKind[k]*100/s.Cents)
}

// biggestChangeNote is the kind whose amount changed the most between two periods (it is in both):
// "💡 Най-много се промени ток: +18,30 € (+23 %).", "" when nothing changed.
func biggestChangeNote(a, b Sum) string {
	var kinds []string
	for k, v := range a.ByKind {
		if w, ok := b.ByKind[k]; ok && w != v {
			kinds = append(kinds, k)
		}
	}
	if len(kinds) == 0 {
		return ""
	}
	abs := func(x int64) int64 {
		if x < 0 {
			return -x
		}
		return x
	}
	sort.Slice(kinds, func(i, j int) bool {
		di, dj := abs(a.ByKind[kinds[i]]-b.ByKind[kinds[i]]), abs(a.ByKind[kinds[j]]-b.ByKind[kinds[j]])
		if di != dj {
			return di > dj
		}
		return kinds[i] < kinds[j]
	})
	k := kinds[0]
	verb := "поскъпна"
	if a.ByKind[k] < b.ByKind[k] {
		verb = "поевтиня"
	}
	return fmt.Sprintf("💡 Най-много %s %s: %s.", verb, KindNames[k], change(a.ByKind[k], b.ByKind[k]))
}

func writeNote(b *strings.Builder, note string) {
	if note != "" {
		b.WriteString(note + "\n")
	}
}
