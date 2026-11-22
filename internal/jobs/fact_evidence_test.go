package jobs

import (
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/metrics"
)

func TestAFactStandsOnlyOnThePersonsOwnWords(t *testing.T) {
	const message = "Запомни че аз съм от България но от 6-7 години живея в Бремен Германия а и ти със мен от скоро 🙂"
	cases := []struct {
		name string
		fact Fact
		want string
	}{
		{"a phrase of the message", Fact{Evidence: "от 6-7 години живея в Бремен", FactKey: "current_location", Value: "Бремен, Германия"}, ""},
		{"other case, spaces, a full stop and quotes around it", Fact{Evidence: " „Аз  съм от БЪЛГАРИЯ.“ ", FactKey: "origin", Value: "България"}, ""},
		{"a dash the model changed", Fact{Evidence: "от 6–7 години", FactKey: "x", Value: "y"}, ""},
		{"no evidence", Fact{FactKey: "origin", Value: "България"}, dropNoEvidence},
		{"evidence too short to show anything", Fact{Evidence: "аз", FactKey: "origin", Value: "България"}, dropNoEvidence},
		{"only punctuation", Fact{Evidence: " ... ", FactKey: "origin", Value: "България"}, dropNoEvidence},
		{"the model's own words", Fact{Evidence: "не разбира кодирането достатъчно добре", FactKey: "skills_level", Value: "слабо"}, dropNotInText},
		{"words of the message, changed", Fact{Evidence: "живее в Бремен", FactKey: "current_location", Value: "Бремен"}, dropNotInText},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, dropReason(message, c.fact), c.name)
	}
	assert.Equal(t, "", dropReason("Любимата ми книга е „Към себе си“ на Марк Аврелий.",
		Fact{Evidence: `Любимата ми книга е "Към себе си" на`, FactKey: "favorite_book", Value: "Към себе си"}),
		"quotes inside the phrase, changed by the model")
}

func TestANameMustBeShortAndInTheMessage(t *testing.T) {
	const message = "Здравей, казвам се Ивайло Петров и съм от Бремен."
	assert.Equal(t, "", dropReason(message, Fact{Evidence: "казвам се Ивайло Петров", FactKey: "name", Value: "Ивайло Петров"}))
	assert.Equal(t, "", dropReason(message, Fact{Evidence: "казвам се Ивайло", FactKey: "Name", Value: "ивайло"}), "the key and the value in any case")
	assert.Equal(t, dropNameNotName, dropReason(message, Fact{Evidence: "казвам се Ивайло", FactKey: "name", Value: "Иван"}), "a name that is not in the message")
	assert.Equal(t, dropNameNotName, dropReason(message, Fact{Evidence: "казвам се Ивайло Петров и съм от Бремен", FactKey: "name", Value: "Ивайло Петров и съм от Бремен"}), "a phrase, not a name")
	assert.Equal(t, dropNameNotName, dropReason(message, Fact{Evidence: "казвам се Ивайло", FactKey: "name", Value: ""}))
	assert.Equal(t, "", dropReason(message, Fact{Evidence: "съм от Бремен", FactKey: "location", Value: "Бремен, Германия"}), "only a name must be in the message itself")

	// The signature under a quote that the real model took for the name (2026-11-22).
	const quote = "Мълчанието е без значение между двама човека ако те се чувстват удобно заедно мълчейки -Ивайло Иванов 🙂😁"
	assert.Equal(t, dropNameNotName, dropReason(quote, Fact{Evidence: "мълчейки -Ивайло Иванов", FactKey: "name", Value: "Ивайло Иванов"}), "a signature is not an introduction")
	assert.Equal(t, "", dropReason("Аз съм Мария, приятно ми е.", Fact{Evidence: "Аз съм Мария", FactKey: "name", Value: "Мария"}))
	assert.Equal(t, "", dropReason("Hi, my name is Anna.", Fact{Evidence: "my name is Anna", FactKey: "name", Value: "Anna"}))
	assert.Equal(t, dropNameNotName, dropReason("Казвам се Ивайло, а брат ми е Петър.", Fact{Evidence: "Казвам се Ивайло, а брат ми е Петър", FactKey: "name", Value: "Петър"}),
		"another name later in the sentence is not the one introduced")
}

func TestGroundedFactsKeepsTheGroundedAndCountsTheRest(t *testing.T) {
	before := droppedCount(t, dropNotInText)
	kept := groundedFacts("Живея в София от 2010 г.", []Fact{
		{Evidence: "Живея в София", FactKey: "location", Value: "София"},
		{Evidence: "обича планините", FactKey: "hobby", Value: "планини"},
	})
	assert.Equal(t, []Fact{{Evidence: "Живея в София", FactKey: "location", Value: "София"}}, kept)
	assert.Equal(t, before+1, droppedCount(t, dropNotInText))
	assert.Empty(t, groundedFacts("Как си?", nil))
}

func droppedCount(t *testing.T, reason string) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, metrics.MemoryFactsDropped.WithLabelValues(reason).Write(&m))
	return m.GetCounter().GetValue()
}
