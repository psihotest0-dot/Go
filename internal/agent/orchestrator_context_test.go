package agent

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"go-monolith/internal/db"
	"go-monolith/internal/retrieval"
)

// The profile facts are in the prompt at every message, so a retrieved memory that
// is one of them is dropped; one that is not stays.
func TestWithoutProfileMemoriesDropsWhatTheProfileAlreadyCarries(t *testing.T) {
	inProfile, outside := uuid.New(), uuid.New()
	profile := []db.Memory{{ID: inProfile, Content: "Ивайло се казва."}}
	retrieved := []retrieval.SearchResult{
		{ID: inProfile, Content: "Ивайло се казва."},
		{ID: outside, Content: "Ивайло има куче."},
	}

	got := withoutProfileMemories(retrieved, profile)

	assert.Equal(t, []retrieval.SearchResult{{ID: outside, Content: "Ивайло има куче."}}, got)
}

func TestWithoutProfileMemoriesLeavesEverythingWhenThereIsNoProfile(t *testing.T) {
	retrieved := []retrieval.SearchResult{{ID: uuid.New(), Content: "a"}, {ID: uuid.New(), Content: "b"}}

	assert.Equal(t, retrieved, withoutProfileMemories(retrieved, nil))
	assert.Empty(t, withoutProfileMemories(nil, []db.Memory{{ID: uuid.New()}}))
}

// With no more memories than the profile holds, every retrieved one is a repeat.
func TestWithoutProfileMemoriesDropsAllWhenTheProfileHoldsThemAll(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	profile := []db.Memory{{ID: a}, {ID: b}}
	retrieved := []retrieval.SearchResult{{ID: b}, {ID: a}}

	assert.Empty(t, withoutProfileMemories(retrieved, profile))
}

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

// TestSourceSnippetIsBounded covers the snippet shown to the client: it must be
// trimmed and stay within the cap even for very long retrieved passages.
func TestSourceSnippetIsBounded(t *testing.T) {
	if got := sourceSnippet("  къс фрагмент  "); got != "къс фрагмент" {
		t.Fatalf("sourceSnippet() = %q, want %q", got, "къс фрагмент")
	}

	long := strings.Repeat("д", maxSourceSnippetChars+50)
	got := sourceSnippet(long)
	if n := utf8.RuneCountInString(got); n != maxSourceSnippetChars {
		t.Fatalf("sourceSnippet() returned %d runes, want %d", n, maxSourceSnippetChars)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated snippet should be marked as truncated")
	}
}

// TestEstimateTokens pins the heuristic down: four bytes per token, so Cyrillic
// text (two bytes per character) costs about twice as much as Latin text for the
// same number of characters.
func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{name: "empty", in: "", want: 0},
		{name: "short latin", in: "abcd", want: 1},
		{name: "latin over boundary", in: "abcde", want: 2},
		{name: "cyrillic four bytes", in: "яя", want: 1},
		{name: "cyrillic six bytes", in: "яяя", want: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := estimateTokens(tt.in); got != tt.want {
				t.Fatalf("estimateTokens(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}

	// Equal character counts must not cost the same in both scripts.
	latin := strings.Repeat("a", 100)
	cyrillic := strings.Repeat("д", 100)
	if estimateTokens(cyrillic) <= estimateTokens(latin) {
		t.Fatalf("cyrillic text must cost more than latin text of equal length: %d vs %d",
			estimateTokens(cyrillic), estimateTokens(latin))
	}
}

func TestTruncateTokens(t *testing.T) {
	if got := truncateTokens("abcdefgh", 1); estimateTokens(got) > 1 {
		t.Fatalf("truncateTokens() = %q, costs %d tokens, want at most 1", got, estimateTokens(got))
	}
	if got := truncateTokens("", 5); got != "" {
		t.Fatalf("truncateTokens(\"\", 5) = %q, want empty", got)
	}
	if got := truncateTokens("abcdef", 0); got != "" {
		t.Fatalf("truncateTokens with no tokens = %q, want empty", got)
	}
	if got := truncateTokens("кратко", 100); got != "кратко" {
		t.Fatalf("truncateTokens should not touch text inside the budget, got %q", got)
	}

	// The bound has to hold for multibyte text as well.
	long := strings.Repeat("д", 5000)
	got := truncateTokens(long, 100)
	if n := estimateTokens(got); n > 100 {
		t.Fatalf("truncateTokens() = %d tokens, want at most 100", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatal("truncated text should be marked as truncated")
	}
}

func TestPromptBudgetKeepsItemsUntilExhausted(t *testing.T) {
	// Four latin characters are one estimated token, so a two token budget
	// keeps exactly two of the four items.
	budget := newPromptBudget(2)

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
	if budget.remaining != 0 {
		t.Fatalf("remaining = %d, want 0", budget.remaining)
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
	// Four Cyrillic characters plus the ellipsis cost eleven bytes, which is
	// three estimated tokens.
	if budget.remaining != 7 {
		t.Fatalf("remaining = %d, want 7", budget.remaining)
	}
}

// TestPromptBudgetDropsItemThatDoesNotFit verifies that the first item which no
// longer fits ends the section, so the prompt never receives a fragment that
// was cut down just to fill the remaining budget.
func TestPromptBudgetDropsItemThatDoesNotFit(t *testing.T) {
	budget := newPromptBudget(1)

	got := budget.take([]string{"abcdefgh"}, 100)

	if len(got) != 0 {
		t.Fatalf("kept %v, want nothing", got)
	}
	if budget.remaining != 1 {
		t.Fatalf("remaining = %d, want 1", budget.remaining)
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
	// The twenty byte rule costs five estimated tokens, leaving a single token
	// that the two token memory cannot use.
	budget := newPromptBudget(6)

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

	kept := newPromptBudget(defaultPromptContextTokens).take(chunks, maxConceptChunkChars)

	if len(kept) == 0 {
		t.Fatal("expected at least one chunk to be kept")
	}

	total := 0
	for _, chunk := range kept {
		if n := utf8.RuneCountInString(chunk); n > maxConceptChunkChars {
			t.Fatalf("kept chunk has %d runes, want at most %d", n, maxConceptChunkChars)
		}
		total += estimateTokens(chunk)
	}
	if total > defaultPromptContextTokens {
		t.Fatalf("kept %d tokens, want at most %d", total, defaultPromptContextTokens)
	}
}
