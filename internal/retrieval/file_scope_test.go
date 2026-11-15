package retrieval

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// library returns the files newest first, as GetIngestedFiles does.
func library(names ...string) []NamedFile {
	files := make([]NamedFile, len(names))
	for i, n := range names {
		files[i] = NamedFile{ID: uuid.New(), Name: n}
	}
	return files
}

// A question names a file when it holds the whole name with its extension, or the title
// and a word that says "file" (or a title with a digit). The cases are the ones measured on
// the real library on 2026-11-15: 0 false alarms in 28 ordinary questions and 46 real
// messages, 119 of 119 files found by the whole name, 117 of 119 by file word + title.
func TestMatchNamedFile(t *testing.T) {
	lib := library(
		"CLBSV2.pdf",
		"Python Cookbook.pdf",
		"modern C.pdf",
		"Modern C++ Programming Cookbook, 2nd Edition - Author - Publisher (2020).pdf",
		"programming.pdf",
		"Go.pdf",
		"Advanced Algorithmic.pdf",
		"Algorithmic Trading from Beginner to Advanced.pdf",
	)
	cases := []struct {
		name, text, want string
	}{
		{"a title with a digit names the file by itself", "какво пише в CLBSV2", "CLBSV2.pdf"},
		{"the case in point", "какво пиши в файла CLBSV2", "CLBSV2.pdf"},
		{"file word and title", "какво пише във файла Python Cookbook", "Python Cookbook.pdf"},
		{"book word and title", "what does the book python cookbook say about csv", "Python Cookbook.pdf"},
		{"the whole name with the extension", "прочети ми Python Cookbook.pdf", "Python Cookbook.pdf"},
		{"case, underscores and dashes do not matter", "прочети ми python_cookbook.PDF", "Python Cookbook.pdf"},
		{"the title as a topic, without a file word, is only words", "рецепти от Python Cookbook", ""},
		{"an ordinary phrase that is also a title", "what is advanced algorithmic trading", ""},
		{"a plain word that is the title of a file", "how do I learn programming", ""},
		{"the extension makes even a plain word a file name", "what is in programming.pdf", "programming.pdf"},
		{"a title that is part of another name needs the whole name", "във файла Modern C++ Programming Cookbook", ""},
		{"the whole name of that file names it", "във файла modern C.pdf", "modern C.pdf"},
		{"a title shorter than four letters is a word", "във файла Go.pdf", ""},
		{"nothing is named", "Здрасти", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := matchNamedFile(lib, c.text)
			if c.want == "" {
				assert.False(t, ok, "named %q", got.Name)
				return
			}
			assert.True(t, ok)
			assert.Equal(t, c.want, got.Name)
		})
	}
}

func TestMatchNamedFileLooksInEveryTextAndSaysFileFromAny(t *testing.T) {
	lib := library("Python Cookbook.pdf")

	got, ok := matchNamedFile(lib, "какво пише в него", "Python Cookbook.pdf")
	assert.True(t, ok, "the search query of the model names the file")
	assert.Equal(t, "Python Cookbook.pdf", got.Name)

	got, ok = matchNamedFile(lib, "какво пише във файла", "Python Cookbook")
	assert.True(t, ok, "the file word is in the message, the title in the query")
	assert.Equal(t, "Python Cookbook.pdf", got.Name)

	_, ok = matchNamedFile(lib, "", "   ")
	assert.False(t, ok)
	_, ok = matchNamedFile(nil, "във файла CLBSV2")
	assert.False(t, ok, "no files")
}

func TestMatchNamedFileTakesTheLongestNameAndRefusesWhenTwoFilesAreMeant(t *testing.T) {
	t.Run("the longer name wins over a name inside it", func(t *testing.T) {
		lib := library("Docker Cookbook Second Edition.pdf", "Docker Cookbook.pdf")
		got, ok := matchNamedFile(lib, "във файла Docker Cookbook Second Edition")
		assert.True(t, ok)
		assert.Equal(t, "Docker Cookbook Second Edition.pdf", got.Name)

		_, ok = matchNamedFile(lib, "във файла Docker Cookbook")
		assert.False(t, ok, "the short title is part of the longer name: ambiguous without the extension")

		got, ok = matchNamedFile(lib, "във файла Docker Cookbook.pdf")
		assert.True(t, ok, "the whole name settles it")
		assert.Equal(t, "Docker Cookbook.pdf", got.Name)
	})

	t.Run("two different files named with equal weight", func(t *testing.T) {
		lib := library("Alpha1.pdf", "Bravo1.pdf")
		_, ok := matchNamedFile(lib, "сравни Alpha1 и Bravo1")
		assert.False(t, ok, "which one? the whole library is searched, as before")
	})

	t.Run("two copies of one name: the newer one", func(t *testing.T) {
		lib := library("Alpha1.pdf", "Alpha1.pdf")
		got, ok := matchNamedFile(lib, "какво пише в Alpha1")
		assert.True(t, ok)
		assert.Equal(t, lib[0].ID, got.ID)
	})
}

func TestMatchNamedFileReadsOnlyTheStartOfALongText(t *testing.T) {
	lib := library("CLBSV2.pdf")
	pasted := strings.Repeat("дълъг текст ", maxNamedFileTextRunes) + " файла CLBSV2"

	_, ok := matchNamedFile(lib, pasted)
	assert.False(t, ok, "a pasted page is not a question about a file named at its far end")

	_, ok = matchNamedFile(lib, "файла CLBSV2 "+pasted)
	assert.True(t, ok)
}

func TestFileTitleCutsOnlyARealExtension(t *testing.T) {
	assert.Equal(t, "CLBSV2", fileTitle("CLBSV2.pdf"))
	assert.Equal(t, "Myths of Greece and Rome", fileTitle("Myths of Greece and Rome.epub"))
	assert.Equal(t, "host.site_learning-go-programming", fileTitle("host.site_learning-go-programming.pdf"))
	assert.Equal(t, "Mr. Smith and others", fileTitle("Mr. Smith and others"), "the dot of a sentence is not an extension")
	assert.Equal(t, "notes", fileTitle("notes"))
}
