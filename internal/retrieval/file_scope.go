package retrieval

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"

	"go-monolith/internal/db"
	"go-monolith/metrics"
)

const (
	// minNamedFileRunes is the shortest title (the file name without its extension, in
	// letters and digits) that can be taken for a file named in a question: shorter ones
	// ("go", "c", "ai") are ordinary words.
	minNamedFileRunes = 4

	// maxNamedFileTextRunes bounds the text that is searched for file names: the user's
	// message can be a pasted page.
	maxNamedFileTextRunes = 1000
)

// fileWord is the word that says a file is meant: "във файла CLBSV2", "the document
// Python Cookbook". Without one the title of a file is only words (a question about "advanced
// algorithmic trading" is not about the file "Advanced Algorithmic.pdf"), unless it holds a digit.
var fileWord = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:файл\p{L}*|документ\p{L}*|книг\p{L}*|pdf|epub|docx|file|files|document|documents|book|books)(?:[^\p{L}]|$)`)

// NamedFile is a file of the library that a question names.
type NamedFile struct {
	ID   uuid.UUID
	Name string
}

// foldName lowercases s and reduces it to its letters and digits, one space between
// the words, so that "CLBS-V2_final.pdf" and "clbs v2 final pdf" compare equal.
func foldName(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}

// fileTitle is the name of a file without its extension (".pdf", ".epub": two to five
// letters or digits after the last dot).
func fileTitle(name string) string {
	ext := path.Ext(name)
	if n := len(ext) - 1; n >= 2 && n <= 5 && !strings.ContainsAny(ext, " _-/\\") {
		return strings.TrimSuffix(name, ext)
	}
	return name
}

// distinctiveTitle tells whether a title (folded) names a file even when the question
// does not say "file": only when it holds a digit ("clbsv2", "psyhology2"), which an
// ordinary phrase seldom does. Every other title needs a file word.
func distinctiveTitle(title string) bool {
	return strings.IndexFunc(title, unicode.IsDigit) >= 0
}

// matchNamedFile returns the file that texts (the user's message, the search query)
// name: the whole file name with its extension ("CLBSV2.pdf"), or its title as
// separate words when the text says "file"/"book" (or the title holds a digit). Of several
// matches the one with the longest name wins; two different files with equally long
// names make the question ambiguous and nothing is returned. files come newest first,
// so of two copies with the same name the newer one is taken.
func matchNamedFile(files []NamedFile, texts ...string) (NamedFile, bool) {
	var hay []string
	saysFile := false
	for _, t := range texts {
		t = truncateRunes(strings.TrimSpace(t), maxNamedFileTextRunes)
		if t == "" {
			continue
		}
		hay = append(hay, " "+foldName(t)+" ")
		saysFile = saysFile || fileWord.MatchString(t)
	}
	if len(hay) == 0 {
		return NamedFile{}, false
	}

	fulls := make([]string, len(files))
	for i, f := range files {
		fulls[i] = " " + foldName(f.Name) + " "
	}
	// partOfAnotherName: the title is also words of another file's name ("modern c" in
	// "Modern C++ Programming Cookbook ..."), so a question that holds it may mean that
	// other file; then only the whole name with its extension names the file.
	partOfAnotherName := func(i int, title string) bool {
		for j, other := range fulls {
			if j != i && other != fulls[i] && strings.Contains(other, " "+title+" ") {
				return true
			}
		}
		return false
	}

	var best NamedFile
	bestTitle, bestRunes, ambiguous := "", 0, false
	for i, f := range files {
		full, title := foldName(f.Name), foldName(fileTitle(f.Name))
		if utf8.RuneCountInString(title) < minNamedFileRunes {
			continue
		}
		matched := ""
		for _, h := range hay {
			switch {
			case strings.Contains(h, " "+full+" "):
				matched = full
			case strings.Contains(h, " "+title+" ") && (saysFile || distinctiveTitle(title)) && !partOfAnotherName(i, title):
				matched = title
			}
			if matched != "" {
				break
			}
		}
		if matched == "" {
			continue
		}
		n := utf8.RuneCountInString(matched)
		switch {
		case n > bestRunes:
			best, bestTitle, bestRunes, ambiguous = f, title, n, false
		case n == bestRunes && title != bestTitle:
			ambiguous = true
		}
	}
	if bestRunes == 0 || ambiguous {
		return NamedFile{}, false
	}
	return best, true
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

// FileNamedIn returns the file of the user's library that texts name, if exactly one
// is named (see matchNamedFile). A failure to read the list is not an error of the
// search: the caller searches the whole library as before.
func (s *RetrievalService) FileNamedIn(ctx context.Context, userID uuid.UUID, texts ...string) (NamedFile, bool) {
	rows, err := s.db.Queries.GetIngestedFiles(ctx, userID)
	if err != nil {
		slog.WarnContext(ctx, "could not list the files to see whether the question names one", "error", err)
		return NamedFile{}, false
	}
	files := make([]NamedFile, 0, len(rows))
	for _, r := range rows {
		if r.Status == "completed" {
			files = append(files, NamedFile{ID: r.ID, Name: r.FileName})
		}
	}
	return matchNamedFile(files, texts...)
}

// SearchFileChunks is SearchRawChunks inside one file: for a question that names the
// file ("what does CLBSV2.pdf say"). Everything that the search of the whole library
// does to keep one book from filling the answer is off here (see SearchFileChunks in
// files.sql), so the answer is the passages of that file, nothing else. Without the
// embedder it has no vector to rank by and searches the whole library as before.
func (s *RetrievalService) SearchFileChunks(ctx context.Context, userID, fileID uuid.UUID, query string, limit int) ([]SearchResult, error) {
	start := time.Now()
	defer func() {
		metrics.StageLatency.WithLabelValues("search_file_chunks").Observe(time.Since(start).Seconds())
	}()

	queryVec, err := s.embedQuery(ctx, query)
	if err != nil {
		return nil, err
	}
	if queryVec == nil {
		return s.searchChunks(ctx, metrics.RetrievalSearchChunks, userID, query, nil, limit, nil)
	}

	var rows []db.SearchFileChunksRow
	err = s.db.WithSearchSettings(ctx, func(q *db.Queries) (err error) {
		rows, err = q.SearchFileChunks(ctx, db.SearchFileChunksParams{
			UserID:     userID,
			FileID:     fileID,
			MatchCount: int32(limit),
			Embedding:  pgvector.NewVector(queryVec),
			TsQuery:    lexicalQuery(query, nil),
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("failed to search the file: %w", err)
	}
	res := make([]SearchResult, 0, len(rows))
	for _, r := range rows {
		res = append(res, SearchResult{ID: r.ID, Content: r.Content, SourceType: "chunk", Score: r.Score, FileName: r.FileName})
	}
	return res, nil
}
