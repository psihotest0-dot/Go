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
// decision for each break must not scan the whole text again. The test compares
// the time for a text with the time for four times as much of it instead of
// holding one run to a number of seconds: a linear join takes about 4 times as
// long, a quadratic one about 16 times, and the ratio does not depend on how
// fast the machine is (a fixed limit of 3 s failed on a slow CI runner under the
// race detector, which makes the code several times slower).
func TestJoinHyphenatedIsLinear(t *testing.T) {
	const paragraph = "Парите на хората се обезцен-\nяват и по-\nголемите спестявания зат-\nворници.\n"

	// bestTime is the fastest of a few runs, which keeps a garbage collection or
	// a busy neighbour from deciding the result. Building the text is not timed.
	bestTime := func(repeats int) (time.Duration, int) {
		text := strings.Repeat(paragraph, repeats)
		best := time.Duration(1<<63 - 1)
		for run := 0; run < 3; run++ {
			start := time.Now()
			got := JoinHyphenated(text)
			if took := time.Since(start); took < best {
				best = took
			}
			if strings.Contains(got, "-\n") {
				t.Fatal("a break was left")
			}
		}
		return best, len(text)
	}

	small, smallChars := bestTime(5000)
	large, largeChars := bestTime(20000)
	t.Logf("%d characters: %s; %d characters: %s", smallChars, small.Round(time.Millisecond), largeChars, large.Round(time.Millisecond))

	// 4x the text: linear is ~4, quadratic ~16. Ten is far from both.
	if large > 10*small {
		t.Fatalf("%d characters took %s but %d took %s: more than 10 times as long for 4 times the text, so it is not linear",
			largeChars, large, smallChars, small)
	}
}
