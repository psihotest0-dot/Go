package agent

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{name: "shorter than max", in: "кратко", max: 10, want: "кратко"},
		{name: "exactly max", in: "12345", max: 5, want: "12345"},
		{name: "longer than max", in: "abcdefgh", max: 5, want: "abcd…"},
		{name: "max one", in: "abcdef", max: 1, want: "…"},
		{name: "max zero", in: "abcdef", max: 0, want: ""},
		{name: "negative max", in: "abcdef", max: -3, want: ""},
		{name: "empty input", in: "", max: 5, want: ""},
		{name: "multibyte cut by runes", in: "абвгдежз", max: 4, want: "абв…"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateRunes(tt.in, tt.max)
			if got != tt.want {
				t.Fatalf("truncateRunes(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
			if tt.max > 0 && utf8.RuneCountInString(got) > tt.max {
				t.Fatalf("truncateRunes(%q, %d) returned %d runes, want at most %d",
					tt.in, tt.max, utf8.RuneCountInString(got), tt.max)
			}
		})
	}
}

func TestPromptBudgetKeepsItemsUntilExhausted(t *testing.T) {
	budget := newPromptBudget(10)

	got := budget.take([]string{"aaaa", "bbbb", "cccc", "dddd"}, 100)

	want := []string{"aaaa", "bbbb"}
	if len(got) != len(want) {
		t.Fatalf("kept %d items (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kept[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if budget.remaining != 2 {
		t.Fatalf("remaining = %d, want 2", budget.remaining)
	}
}

func TestPromptBudgetTruncatesOversizedItem(t *testing.T) {
	budget := newPromptBudget(10)

	got := budget.take([]string{strings.Repeat("я", 100)}, 5)

	if len(got) != 1 {
		t.Fatalf("kept %d items, want 1", len(got))
	}
	if n := utf8.RuneCountInString(got[0]); n != 5 {
		t.Fatalf("kept item has %d runes, want 5 (%q)", n, got[0])
	}
	if !strings.HasSuffix(got[0], "…") {
		t.Fatalf("kept item %q should be marked as truncated", got[0])
	}
	if budget.remaining != 5 {
		t.Fatalf("remaining = %d, want 5", budget.remaining)
	}
}

// TestPromptBudgetDropsItemThatDoesNotFit verifies that the first item which no
// longer fits ends the section, so the prompt never receives a fragment that
// was cut down just to fill the remaining budget.
func TestPromptBudgetDropsItemThatDoesNotFit(t *testing.T) {
	budget := newPromptBudget(3)

	got := budget.take([]string{"abcdefgh"}, 100)

	if len(got) != 0 {
		t.Fatalf("kept %v, want nothing", got)
	}
	if budget.remaining != 3 {
		t.Fatalf("remaining = %d, want 3", budget.remaining)
	}
}

func TestPromptBudgetWithNoBudget(t *testing.T) {
	budget := newPromptBudget(0)

	if got := budget.take([]string{"a"}, 10); len(got) != 0 {
		t.Fatalf("kept %v, want nothing", got)
	}
}

func TestPromptBudgetEmptyInput(t *testing.T) {
	budget := newPromptBudget(100)

	if got := budget.take(nil, 10); got != nil {
		t.Fatalf("take(nil) = %v, want nil", got)
	}
	if budget.remaining != 100 {
		t.Fatalf("remaining = %d, want 100", budget.remaining)
	}
}

// TestPromptBudgetIsSharedBetweenSections verifies that the sections of the
// system prompt draw from one budget: once the rules have consumed it, the
// lower priority sections get nothing.
func TestPromptBudgetIsSharedBetweenSections(t *testing.T) {
	budget := newPromptBudget(20)

	rules := budget.take([]string{strings.Repeat("rule ", 4)}, maxRuleChars)
	if len(rules) != 1 {
		t.Fatalf("expected the rule to fit into the budget, got %v", rules)
	}
	if memories := budget.take([]string{"memory"}, maxRetrievedMemoryChars); len(memories) != 0 {
		t.Fatalf("expected memories to be dropped once the budget is spent, got %v", memories)
	}
}

// TestPromptBudgetBoundsConceptChunks mirrors what loadContext does with the
// retrieved concept chunks, which are the largest unbounded input: even a query
// that matches many long passages must stay inside the budget.
func TestPromptBudgetBoundsConceptChunks(t *testing.T) {
	chunks := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		chunks = append(chunks, strings.Repeat("концепт ", 500))
	}

	kept := newPromptBudget(maxSystemPromptContextChars).take(chunks, maxConceptChunkChars)

	if len(kept) == 0 {
		t.Fatal("expected at least one chunk to be kept")
	}

	total := 0
	for _, chunk := range kept {
		n := utf8.RuneCountInString(chunk)
		if n > maxConceptChunkChars {
			t.Fatalf("kept chunk has %d runes, want at most %d", n, maxConceptChunkChars)
		}
		total += n
	}
	if total > maxSystemPromptContextChars {
		t.Fatalf("kept %d runes, want at most %d", total, maxSystemPromptContextChars)
	}
}
