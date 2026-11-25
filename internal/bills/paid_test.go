package bills

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The bill "платих …" names (the code review of 2026-11-25): a number only as the lists say it.
func TestTheBillAPaidMessageNames(t *testing.T) {
	unpaid := []Bill{
		{ID: 1, Issuer: "Muster Versicherung", Kind: "insurance", Cents: 12000},
		{ID: 2, Issuer: "Beispiel Mobilfunk", Kind: "phone_internet", Cents: 3999},
		{ID: 3, Issuer: "Stadtwerke Beispiel", Kind: "electricity", Cents: 8000},
	}
	for m, want := range map[string][]int{
		"платих тока":                     {2},
		"платих тока на 2-ри":             {2},
		"платих тока в 1 часа":            {2},
		"платих сметката за тока на 3-ти": {2},
		"платих 50 евро за тока":          {2},
		"платих телефона вчера в 2":       {1},
		"платих 2":                        {1},
		"платих 2.":                       {1},
		"вчера платих 2":                  {1},
		"платих сметка 3":                 {2},
		"платих №1":                       {0},
		"платих номер 1":                  {0},
		"платих сметката номер 3 вчера":   {2},
		// A number that is no place of the list, and words that name no bill: the code asks which.
		"платих 3 сметки днес": nil,
		"платих 7":             nil,
	} {
		assert.Equal(t, want, pickBills(unpaid, m), m)
	}
}
