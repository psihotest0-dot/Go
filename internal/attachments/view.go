package attachments

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// DefaultReadLines and MaxReadLines are how many lines one read_attachment call
	// returns by default and at most.
	DefaultReadLines = 200
	MaxReadLines     = 400
	// MaxReadBytes caps one reading by size too, about 3 000 tokens of code: the
	// results of a round of tool calls share a budget of 4 000.
	MaxReadBytes = 9000

	lineCutMarker = " [line cut]"
)

// numbered is one line with its number, the way the model sees a file: "  12| text".
// The number is not part of the file; the prompt says so.
func numbered(n int, text string) string {
	if utf8.RuneCountInString(text) > MaxLineChars {
		text = string([]rune(text)[:MaxLineChars]) + lineCutMarker
	}
	return fmt.Sprintf("%4d| %s", n, text)
}

// Inline renders a whole file for the prompt, every line with its number.
func Inline(f File) string {
	lines := SplitLines(f.Content)
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = numbered(i+1, l)
	}
	return strings.Join(out, "\n")
}

// Slice is a part of a file, as the read_attachment tool returns it.
type Slice struct {
	Name       string `json:"name"`
	TotalLines int    `json:"total_lines"`
	// StartLine and EndLine are the first and last line returned (EndLine is
	// StartLine-1 when the file ends before StartLine).
	StartLine int `json:"start_line"`
	EndLine   int `json:"end_line"`
	// NextStartLine is where to continue; 0 when the end of the file was reached.
	NextStartLine int    `json:"next_start_line"`
	Content       string `json:"content"`
}

// Read returns up to maxLines lines of the file from line start (1-based), numbered, and
// no more than MaxReadBytes. At least one line is returned when there is one.
func Read(f File, start, maxLines int) Slice {
	lines := SplitLines(f.Content)
	total := len(lines)
	if start < 1 {
		start = 1
	}
	switch {
	case maxLines <= 0:
		maxLines = DefaultReadLines
	case maxLines > MaxReadLines:
		maxLines = MaxReadLines
	}

	s := Slice{Name: f.Name, TotalLines: total, StartLine: start, EndLine: start - 1}
	if start > total {
		return s
	}

	var b strings.Builder
	for i := start - 1; i < total && i-(start-1) < maxLines; i++ {
		line := numbered(i+1, lines[i])
		if i > start-1 && b.Len()+len(line)+1 > MaxReadBytes {
			break
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		s.EndLine = i + 1
	}
	s.Content = b.String()
	if s.EndLine < total {
		s.NextStartLine = s.EndLine + 1
	}
	return s
}

const (
	// MaxFindMatches is how many matching lines one search returns; MaxMatchChars cuts a
	// long matching line.
	MaxFindMatches = 40
	MaxMatchChars  = 240
)

// Match is a line that contains the searched text.
type Match struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

// Found is the answer to a search in one file.
type Found struct {
	Name         string  `json:"name"`
	TotalLines   int     `json:"total_lines"`
	TotalMatches int     `json:"total_matches"`
	Matches      []Match `json:"matches"`
}

// FindText lists the lines of the file that contain text, in any letter case, with their
// numbers: the way to locate a function or a word in a file that is too big to read
// through. At most MaxFindMatches lines are returned; TotalMatches counts all of them.
func FindText(f File, text string) Found {
	lines := SplitLines(f.Content)
	out := Found{Name: f.Name, TotalLines: len(lines), Matches: []Match{}}
	needle := strings.ToLower(strings.TrimSpace(text))
	if needle == "" {
		return out
	}
	for i, l := range lines {
		if !strings.Contains(strings.ToLower(l), needle) {
			continue
		}
		out.TotalMatches++
		if len(out.Matches) < MaxFindMatches {
			if utf8.RuneCountInString(l) > MaxMatchChars {
				l = string([]rune(l)[:MaxMatchChars]) + lineCutMarker
			}
			out.Matches = append(out.Matches, Match{Line: i + 1, Text: strings.TrimSpace(l)})
		}
	}
	return out
}
