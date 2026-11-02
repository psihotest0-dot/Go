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
