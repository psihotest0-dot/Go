package userfiles

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateNameAcceptsOrdinaryNames(t *testing.T) {
	for _, name := range []string{
		"notes.txt",
		"Бележки.md",
		"списък за пазаруване.txt",
		"Plan (2).json",
		"a.b.yaml",
		"data.CSV",
		"x.YML",
		"report-2026_11.log",
		"1.txt",
		strings.Repeat("a", 96) + ".txt", // 100 characters, the longest
	} {
		assert.NoError(t, ValidateName(name), name)
	}
}

// Each of these has been used, somewhere, to get a program to open or write something other
// than what it meant to. None of them may get as far as the disk.
func TestValidateNameRefusesHostileNames(t *testing.T) {
	hostile := map[string]string{
		"empty":                          "",
		"spaces only":                    " ",
		"dot dot":                        "..",
		"up and out":                     "../x.txt",
		"up and out, Windows":            `..\x.txt`,
		"absolute":                       "/etc/passwd.txt",
		"a folder":                       "a/b.txt",
		"a folder, Windows":              `a\b.txt`,
		"a drive":                        `C:\x.txt`,
		"a drive, relative":              "C:x.txt",
		"a network share":                `\\server\share\x.txt`,
		"an NTFS stream":                 "x.txt:stream",
		"the default NTFS stream":        "x.txt::$DATA",
		"device CON":                     "CON.txt",
		"device con, lower case":         "con.txt",
		"device Nul":                     "Nul.md",
		"device COM1":                    "COM1.txt",
		"device lpt9":                    "lpt9.csv",
		"device with more dots":          "con.backup.txt",
		"device with a space":            "nul .txt",
		"superscript COM":                "com\u00b9.txt",
		"ends with a dot":                "x.txt.",
		"ends with a space":              "x.txt ",
		"starts with a space":            " x.txt",
		"starts with a dot":              ".hidden.txt",
		"starts with a dash":             "-x.txt",
		"starts with an underscore":      "_x.txt",
		"an 8.3 short name":              "PROGRA~1.txt",
		"two dots in a row":              "a..b.txt",
		"a script":                       "x.ps1",
		"a batch file":                   "x.bat",
		"a command file":                 "x.cmd",
		"a shortcut":                     "x.lnk",
		"an internet shortcut":           "x.url",
		"a web page":                     "x.html",
		"JavaScript":                     "x.js",
		"Python":                         "x.py",
		"a registry file":                "x.reg",
		"a program":                      "x.exe",
		"no extension":                   "x",
		"only an extension":              ".txt",
		"a harmless name on a harmful":   "x.txt.exe",
		"a control character":            "x\n.txt",
		"a NUL":                          "x\x00.txt",
		"a right-to-left override":       "x\u202etxt.exe",
		"an emoji":                       "😀.txt",
		"a star":                         "a*b.txt",
		"a question mark":                "a?b.txt",
		"a quote":                        `a"b.txt`,
		"a less-than sign":               "a<b.txt",
		"a greater-than sign":            "a>b.txt",
		"a pipe":                         "a|b.txt",
		"not UTF-8":                      "\xff.txt",
		"one character too many":         strings.Repeat("a", 97) + ".txt",
		"a long name made of folders":    strings.Repeat("a/", 60) + "b.txt",
		"a comma (a list in a shell)":    "a,b.txt",
		"a semicolon (a command)":        "a;b.txt",
		"an ampersand (a command)":       "a&b.txt",
		"a dollar (a variable)":          "$HOME.txt",
		"a percent (a variable)":         "%PATH%.txt",
		"backticks (a command)":          "`id`.txt",
		"a leading dollar and a stream":  "$MFT.txt",
		"a hash":                         "a#b.txt",
		"an at sign":                     "a@b.txt",
		"a plus sign (a mailbox)":        "a+b.txt",
		"an equals sign":                 "a=b.txt",
		"square brackets (a glob)":       "a[1].txt",
		"curly braces (a glob)":          "a{1}.txt",
		"an apostrophe":                  "a'b.txt",
		"an exclamation mark":            "a!b.txt",
		"a caret":                        "a^b.txt",
		"a tab":                          "a\tb.txt",
		"a zero width joiner":            "a\u200db.txt",
		"a zero width space":             "a\u200bb.txt",
		"a non-breaking space":           "a\u00a0b.txt",
		"a full width solidus":           "a\uff0fb.txt",
		"a combining mark alone":         "\u0301.txt",
		"a name with only dots and text": "...txt",
	}
	for why, name := range hostile {
		err := ValidateName(name)
		var invalid *InvalidError
		require.ErrorAs(t, err, &invalid, "%s: %q must be refused", why, name)
		assert.NotEmpty(t, invalid.Reason, why)
	}
}

// The reason goes to the model, which has to be able to fix the call from it.
func TestValidateNameSaysWhatToDo(t *testing.T) {
	assert.Contains(t, ValidateName("x.exe").Error(), ".txt")
	assert.Contains(t, ValidateName("x").Error(), "extension")
	assert.Contains(t, ValidateName("a/b.txt").Error(), "folders")
	assert.Contains(t, ValidateName("CON.txt").Error(), "reserved")
	assert.Contains(t, ValidateName(strings.Repeat("a", 120)+".txt").Error(), "100")
	assert.Contains(t, ValidateName("x.txt.").Error(), "end with a dot")
	assert.Contains(t, ValidateName("x.txt ").Error(), "end with a dot or a space")
	assert.Contains(t, ValidateName("x\n.txt").Error(), "control character")
	assert.NotContains(t, ValidateName("x\n.txt").Error(), "\n", "a control character is not echoed back")
}
