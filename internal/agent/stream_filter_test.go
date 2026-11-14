package agent

import (
	"strings"
	"testing"
)

func TestStreamFilter(t *testing.T) {
	filter := &streamFilter{}

	input := []string{
		"Hello ",
		"<thi",
		"nk>This is a thought</t",
		"hink> World!",
		" <function=answer>",
		" <parameter=query>Иво",
		"</parameter> </function> ",
		"</tool_call>",
	}

	var out strings.Builder
	for _, chunk := range input {
		out.WriteString(filter.Process(chunk))
	}
	out.WriteString(filter.Flush())

	expected := "Hello  World!  Иво  "
	if out.String() != expected {
		t.Errorf("Expected %q, got %q", expected, out.String())
	}
}

// The live stream cannot take back what it sent, but it must never show the tag.
func TestStreamFilterSwallowsAClosingTagWithoutAnOpeningOne(t *testing.T) {
	filter := &streamFilter{}

	var out strings.Builder
	for _, chunk := range []string{"Отго", "вор</th", "ink>край"} {
		out.WriteString(filter.Process(chunk))
	}
	out.WriteString(filter.Flush())

	if out.String() != "Отговоркрай" {
		t.Errorf("got %q, want %q", out.String(), "Отговоркрай")
	}
}

func TestSanitizeAssistantText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain text is unchanged", in: "Здравей, как си?", want: "Здравей, как си?"},
		{name: "think block at the start", in: "<think>скрити мисли</think>\n\nОтговорът е 42.", want: "Отговорът е 42."},
		{name: "think block in the middle", in: "Първо. <think>хм</think> После.", want: "Първо.  После."},
		{name: "several think blocks", in: "<think>a</think>Едно<think>б</think> две", want: "Едно две"},
		{name: "unterminated think block is cut", in: "Видим текст<think>мисли, които никога не свършват", want: "Видим текст"},
		{name: "only thinking leaves nothing", in: "<think>само мисли</think>", want: ""},
		{name: "function and parameter tags", in: "Търся.<function=search_documents><parameter=query>Дунав</parameter></function>", want: "Търся.Дунав"},
		{name: "tool_call wrapper", in: "<tool_call>\n<function=x></function>\n</tool_call>Готово", want: "Готово"},
		{name: "angle brackets that are not tags survive", in: "ако a < b и c > d, то <b>удебелено</b>", want: "ако a < b и c > d, то <b>удебелено</b>"},
		{name: "empty", in: "", want: ""},
		// The 2026-11-01 Telegram message: a draft and a thought, a lone </think>,
		// then the real answer.
		{name: "reasoning before a lone closing tag is dropped", in: "Чернова.\n(Помислям дали да запиша.)\n</think>\nОтговорът.", want: "Отговорът."},
		{name: "lone closing tag with nothing after it", in: "само мисли</think>", want: ""},
		{name: "several lone closing tags keep the last answer", in: "а</think>б</think>в", want: "в"},
		{name: "lone closing tag after a finished block is only swallowed", in: "<think>а</think>Едно</think> две", want: "Едно две"},
		{name: "closing tag after an opening one is a normal block", in: "Преди <think>мисли</think> след", want: "Преди  след"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeAssistantText(tc.in); got != tc.want {
				t.Errorf("sanitizeAssistantText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Gemma 4 writes a call as <|tool_call>call:name{...}<tool_call|>. When no tool is on offer
// (the last round) it ends up in the answer, and the user got the call itself.
func TestStreamFilterDropsAToolCallWrittenAsText(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"in one piece", []string{`Добавям. <|tool_call>call:append_file{content:"x",name:"a.txt"}<tool_call|> Край`}, "Добавям.  Край"},
		{"split inside the markers and the body", []string{"Добавям. <|tool", "_call>call:append_file{con", `tent:"x"}<tool_`, "call|> Край"}, "Добавям.  Край"},
		{"one character at a time", strings.Split(`а<|tool_call>call:f{}<tool_call|>б`, ""), "аб"},
		{"never closed", []string{`Добавям. <|tool_call>call:append_file{content:"x"`}, "Добавям. "},
		{"two calls", []string{`<|tool_call>call:a{}<tool_call|><|tool_call>call:b{}<tool_call|>Готово`}, "Готово"},
		{"only the call", []string{`<|tool_call>call:a{}<tool_call|>`}, ""},
		{"a body that holds angle brackets", []string{`<|tool_call>call:a{content:"x < y > z"}<tool_call|>ok`}, "ok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := &streamFilter{}
			var out strings.Builder
			for _, chunk := range tt.chunks {
				out.WriteString(filter.Process(chunk))
			}
			out.WriteString(filter.Flush())
			if out.String() != tt.want {
				t.Errorf("got %q, want %q", out.String(), tt.want)
			}
			if got := sanitizeAssistantText(strings.Join(tt.chunks, "")); got != strings.TrimSpace(tt.want) {
				t.Errorf("sanitizeAssistantText: got %q, want %q", got, tt.want)
			}
		})
	}
}

// DeepSeek writes its own call as text when no tool is on offer: <｜｜DSML｜｜ calls> ... </｜｜DSML｜｜ calls>
// with full-width bars. 5 of 79 turns of the comparison of 2026-11-14 ended with it as the answer.
func TestStreamFilterDropsDeepSeeksToolCallWrittenAsText(t *testing.T) {
	call := "<｜｜DSML｜｜ calls>\n<｜｜DSML｜｜ invoke name=\"search_documents\">\n" +
		"<｜｜DSML｜｜ parameter name=\"query\" string=\"true\">отрицателни страни</｜｜DSML｜｜ parameter>\n" +
		"</｜｜DSML｜｜ invoke>\n</｜｜DSML｜｜ calls>"
	tests := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"the whole answer", []string{"\n\n" + call}, "\n\n"},
		{"after words", []string{"Търся ги. " + call + " Готово."}, "Търся ги.  Готово."},
		{"one byte at a time", strings.Split(call+"край", ""), "край"},
		{"split inside a full-width bar", []string{"<\xef\xbd", "\x9c｜DSML｜｜ calls>x</｜｜DSML｜｜ calls>ok"}, "ok"},
		{"never closed", []string{"Текст <｜｜DSML｜｜ calls>\n<｜｜DSML｜｜ invoke name=\"a\">"}, "Текст "},
		{"the other spelling", []string{"<｜DSML｜tool_calls>x</｜DSML｜tool_calls>Готово"}, "Готово"},
		{"a less-than sign that is not a call", []string{"ако a < b и b > c"}, "ако a < b и b > c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := &streamFilter{}
			var out strings.Builder
			for _, chunk := range tt.chunks {
				out.WriteString(filter.Process(chunk))
			}
			out.WriteString(filter.Flush())
			if out.String() != tt.want {
				t.Errorf("got %q, want %q", out.String(), tt.want)
			}
		})
	}
	if got := sanitizeAssistantText("\n\n" + call); got != "" {
		t.Errorf("sanitizeAssistantText of only a call = %q, want nothing", got)
	}
}

func TestStreamFilterResetForgetsAnOpenToolCall(t *testing.T) {
	filter := &streamFilter{}
	filter.Process(`<|tool_call>call:a{`)
	filter.Reset()
	if got := filter.Process("Привет") + filter.Flush(); got != "Привет" {
		t.Errorf("got %q", got)
	}
}

func TestFirstMarkerIsTheEarliestOfTheClosers(t *testing.T) {
	text := "a</｜DSML｜tool_calls>b</｜｜DSML｜｜ calls>c"

	start, end := firstMarker(text, streamFilterDSMLClose)

	if text[start:end] != "</｜DSML｜tool_calls>" {
		t.Errorf("got %q, want the closer that comes first", text[start:end])
	}
	if s, e := firstMarker("nothing here", streamFilterDSMLClose); s != -1 || e != 0 {
		t.Errorf("no marker: got %d, %d", s, e)
	}
}
