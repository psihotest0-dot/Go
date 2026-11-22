package retrieval

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestTheQueryOfAShortMessageHoldsTheEarlierMessages(t *testing.T) {
	earlier := []string{"цяла седмица съм нощем", "днес пак нощна смяна", "трето", "четвърто"}
	assert.Equal(t, "а как да се справя?\nцяла седмица съм нощем\nднес пак нощна смяна\nтрето",
		conversationQuery("а как да се справя?", earlier), "the message first, then at most three earlier ones, the most recent first")
	assert.Equal(t, "", conversationQuery("а как да се справя?", nil), "nothing earlier: nothing to add")
	assert.Equal(t, "", conversationQuery("а как да се справя?", []string{"  ", ""}))
	assert.Equal(t, "", conversationQuery(strings.Repeat("дълго ", 7), earlier), "a message of 40 characters or more says enough itself")
	assert.Equal(t, "", conversationQuery("   ", earlier))

	long := conversationQuery("и?", []string{strings.Repeat("а", 1000)})
	assert.Equal(t, "и?\n"+strings.Repeat("а", maxEarlierMessageRunes), long, "a long earlier message is cut")
	assert.Equal(t, "и?\nедно две", conversationQuery("и?", []string{" едно\n\n две "}), "the spaces of an earlier message are made one")
}

func TestTwoSearchesAreTakenInTurn(t *testing.T) {
	id := func() uuid.UUID { return uuid.New() }
	a1, a2, a3, b1, b2 := id(), id(), id(), id(), id()
	r := func(ids ...uuid.UUID) []SearchResult {
		out := make([]SearchResult, len(ids))
		for i, x := range ids {
			out[i] = SearchResult{ID: x}
		}
		return out
	}
	ids := func(res []SearchResult) []uuid.UUID {
		out := make([]uuid.UUID, len(res))
		for i, x := range res {
			out[i] = x.ID
		}
		return out
	}
	assert.Equal(t, []uuid.UUID{a1, b1, a2, b2, a3}, ids(inTurn(r(a1, a2, a3), r(b1, b2), 5)))
	assert.Equal(t, []uuid.UUID{a1, b1, a2}, ids(inTurn(r(a1, a2, a3), r(b1, a1, b2), 3)), "a memory found by both comes once; the limit holds")
	assert.Equal(t, []uuid.UUID{b1, b2}, ids(inTurn(nil, r(b1, b2), 5)))
	assert.Empty(t, inTurn(nil, nil, 5))
}
