package answerplan

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSkipReason(t *testing.T) {
	for _, tc := range []struct {
		text string
		s    Signals
		want string
	}{
		// Measured by the detector, short or not.
		{"уморен съм", Signals{}, ""},
		{"Как си днес", Signals{}, ""},
		{"не можах да спя добре", Signals{}, ""},
		{"Ще си почина малко сега. Ще се чуем после", Signals{}, ""},
		{"Добро утро, днес съм капнал от умора", Signals{}, ""},
		// Almost no letters.
		{"?", Signals{}, SkipShort},
		{"ок", Signals{}, SkipShort},
		{"👍👍", Signals{}, SkipShort},
		// Courtesy alone, or a farewell with almost nothing else.
		{"Добро утро 🙂", Signals{}, SkipGreeting},
		{"благодаря!", Signals{}, SkipGreeting},
		{"Окей, лека нощ", Signals{}, SkipGreeting},
		{"чао", Signals{}, SkipGreeting},
		{"не знам още, до скоро", Signals{}, SkipGreeting},
		{"ок, до утре", Signals{}, SkipGreeting},
		// Courtesy with something else: the detector decides.
		{"Добро утро, уморен съм", Signals{}, ""},
		{"благодаря много", Signals{}, ""},
		{"добре съм", Signals{}, ""},
		{"Здравей Лили!", Signals{}, ""},
		// A word that only contains a greeting is not one.
		{"окончателно реших", Signals{}, ""},
		{"хейтърите пак пишат", Signals{}, ""},
		{"алое", Signals{}, ""},
		{"Чаот", Signals{}, ""},
		// The content of the message.
		{"виж https://example.com, уморен съм", Signals{}, SkipLink},
		{"уморен съм от www.example.com", Signals{}, SkipLink},
		{"уморен съм от това:\n```go\nx := 1\n```", Signals{}, SkipCode},
		// The signals of the turn go first.
		{"уморен съм", Signals{Cloud: true}, SkipCloud},
		{"уморен съм", Signals{Task: true}, SkipTask},
		{"уморен съм", Signals{ForcedTool: true}, SkipForcedTool},
		{"уморен съм", Signals{FileRequest: true}, SkipFile},
		{"уморен съм", Signals{Attachment: true}, SkipAttachment},
		{"уморен съм", Signals{StatusQuestion: true}, SkipStatus},
		{"https://x", Signals{Cloud: true, ForcedTool: true}, SkipCloud},
	} {
		assert.Equal(t, tc.want, SkipReason(tc.text, tc.s), tc.text)
	}
}
