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
