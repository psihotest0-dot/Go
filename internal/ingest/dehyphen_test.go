package ingest

import (
	"strings"
	"testing"
	"time"
)

func TestJoinHyphenated(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"a syllable break is joined", "ръцете на арестувани или зат-\nворници.", "ръцете на арестувани или затворници."},
		{"several breaks", "белетрис-\nтичен прил. и белетристи-\nчески прил.", "белетристичен прил. и белетристически прил."},
		{"CRLF and the indentation of the next line", "свърза-\r\n    ни гривни", "свързани гривни"},
		{"trailing spaces before the line break", "ор- \t\nганизъм", "организъм"},
		{"a break inside a paragraph, the rest of the text untouched", "първи ред\nвтори сло-\nво трети\n\nнов абзац", "първи ред\nвтори слово трети\n\nнов абзац"},
		{"the other letters of the alphabets work too", "inter-\nnational", "international"},

		// What must stay.
		{"a hyphen before a blank line ends a paragraph", "край на абзаца-\n\nНов абзац", "край на абзаца-\n\nНов абзац"},
		{"a dash with a space is punctuation", "дума -\nпродължение", "дума -\nпродължение"},
		{"an en dash is punctuation", "дума –\nпродължение", "дума –\nпродължение"},
		{"a hyphen inside a line", "бело- Първа част на сложни думи", "бело- Първа част на сложни думи"},
		{"a number after the hyphen", "COVID-\n19 е", "COVID-\n19 е"},
		{"no letter before the hyphen", "1-\nдва", "1-\nдва"},
		{"nothing to do", "Обикновен текст.\nВтори ред.", "Обикновен текст.\nВтори ред."},
		{"empty", "", ""},

		// The break is a hyphen of the language, not of the typesetter.
		{"a name stays hyphenated", "Ана-\nМария дойде", "Ана-Мария дойде"},
		{"a comparative keeps its hyphen", "по-\nголям и най-\nдобър", "по-голям и най-добър"},
		{"a comparative keeps it in any case", "По-\nголям", "По-голям"},

		// The rest of the text decides when the break is ambiguous.
		{"the joined word occurs elsewhere: one word", "да побелеят. Тъканите по-\nбелеят бавно.", "да побелеят. Тъканите побелеят бавно."},
		{"the hyphenated word occurs elsewhere: a compound", "Северо-запад е посока. Вятър от северо-\nзапад духа.", "Северо-запад е посока. Вятър от северо-запад духа."},
		{"capitals do not matter when looking", "Побелели. И те по-\nбелеят: ПОБЕЛЕЯТ.", "Побелели. И те побелеят: ПОБЕЛЕЯТ."},
		{"the break itself is not evidence", "зат-\nворници", "затворници"},

		// An invisible hyphen is never a real one.
		{"soft hyphen at the end of a line", "зат­\nворници", "затворници"},
		{"soft hyphen inside a word", "ин­формация", "информация"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := JoinHyphenated(tt.in); got != tt.want {
				t.Fatalf("\n got  %q\n want %q", got, tt.want)
			}
		})
	}
}

func TestJoinHyphenatedIsIdempotent(t *testing.T) {
	in := "зат-\nворници, по-\nголям, Ана-\nМария, северо-\nзапад и северо-запад."
	once := JoinHyphenated(in)
	if twice := JoinHyphenated(once); twice != once {
		t.Fatalf("not idempotent:\n once  %q\n twice %q", once, twice)
	}
}

// A book of 600 thousand characters must not take noticeable time, and the
// decision for each break must not scan the whole text again.
func TestJoinHyphenatedIsLinear(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20000; i++ {
		b.WriteString("Парите на хората се обезцен-\nяват и по-\nголемите спестявания зат-\nворници.\n")
	}
	text := b.String()
	start := time.Now()
	got := JoinHyphenated(text)
	took := time.Since(start)
	if strings.Contains(got, "-\n") {
		t.Fatal("a break was left")
	}
	if took > 3*time.Second {
		t.Fatalf("took %s for %d characters", took, len(text))
	}
	t.Logf("%d characters, 60000 breaks: %s", len(text), took.Round(time.Millisecond))
}
