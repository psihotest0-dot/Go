package ingest

import (
	"strings"
)

// ChunkText splits text into chunks that respect the token limit of models like paraphrase-multilingual-MiniLM-L12-v2.
// It limits chunks to roughly 400 characters (suitable for Cyrillic where ~300-500 chars = 128 tokens)
// and uses an overlap of about 50 characters to preserve context.
func ChunkText(text string) []string {
	const (
		maxChars = 400
		overlap  = 50
	)

	// Clean up text
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSpace(text)

	if text == "" {
		return nil
	}

	var chunks []string
	runes := []rune(text)
	totalRunes := len(runes)

	for i := 0; i < totalRunes; {
		end := i + maxChars
		if end > totalRunes {
			end = totalRunes
		} else {
			// Try to find a nice breaking point (space or punctuation) near the end
			// Search backwards from the end up to overlap chars
			breakPoint := end
			for j := end - 1; j > i+(maxChars-overlap*2); j-- {
				if isBreakChar(runes[j]) {
					breakPoint = j + 1 // Include the break character
					break
				}
			}
			end = breakPoint
		}

		chunks = append(chunks, string(runes[i:end]))

		if end == totalRunes {
			break
		}

		// Move forward, but keep some overlap
		i = end - overlap
		if i < 0 {
			i = 0
		}

		// If we somehow didn't advance, force it to prevent infinite loops
		if end <= i {
			i = end
		}
	}

	return chunks
}

func isBreakChar(r rune) bool {
	return r == ' ' || r == '\n' || r == '.' || r == '?' || r == '!' || r == ',' || r == ';'
}
