package userfiles

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxNameRunes bounds a file name. With the folder (see MaxFolderLen) and the user's
	// directory in front of it the whole path stays well under the old Windows limit.
	MaxNameRunes = 100
)

// AllowedExtensions are the only kinds of file the model can create or read: plain
// text that nothing runs when it is opened. Scripts, shortcuts and web pages are not on
// the list on purpose (.py, .js, .html, .bat, .cmd, .ps1, .vbs, .lnk, .url, .reg).
var AllowedExtensions = map[string]bool{
	".txt":  true,
	".md":   true,
	".csv":  true,
	".json": true,
	".yaml": true,
	".yml":  true,
	".log":  true,
}

// InvalidError is a request the model got wrong; its reason says how to fix the call,
// in words the model can act on.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return e.Reason }

func invalid(format string, args ...any) error {
	return &InvalidError{Reason: fmt.Sprintf(format, args...)}
}

// reservedDeviceNames are the names Windows treats as devices, with or without an
// extension (NUL.txt is the NUL device). Root refuses them on Windows as well; here they
// are refused everywhere, so a file made on one system can be opened on the other.
var reservedDeviceNames = func() map[string]bool {
	names := map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}
	for i := '1'; i <= '9'; i++ {
		names["COM"+string(i)] = true
		names["LPT"+string(i)] = true
	}
	return names
}()

// ValidateName checks a file name before anything touches the disk. The name is a
// single plain name in one flat folder, from a short list of characters: letters of any
// alphabet, digits, space, "-", "_", "." and round brackets. No "/", "\", ":" (a drive
// or an NTFS stream), "~" (an 8.3 short name), "*", "?", quotes, "<", ">", "|" or
// control characters; it starts with a letter or a digit, does not end with a dot or a
// space (Windows drops those silently), has no "..", is not a device name, and ends
// with one of the AllowedExtensions.
//
// Nothing here is a substitute for os.Root, which is what keeps a name inside the
// folder; this is what keeps the names ordinary.
func ValidateName(name string) error {
	if name == "" {
		return invalid("the file name is empty")
	}
	if !utf8.ValidString(name) {
		return invalid("the file name is not valid text")
	}
	if n := utf8.RuneCountInString(name); n > MaxNameRunes {
		return invalid("the file name is %d characters long; the limit is %d", n, MaxNameRunes)
	}

	for _, r := range name {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
		case r == ' ', r == '-', r == '_', r == '.', r == '(', r == ')':
		default:
			return invalid("the file name has a character that is not allowed (%s); use letters, digits, spaces, \"-\", \"_\" and \".\" only, and no folders", describeRune(r))
		}
	}

	first, _ := utf8.DecodeRuneInString(name)
	if !unicode.IsLetter(first) && !unicode.IsDigit(first) {
		return invalid("the file name must start with a letter or a digit")
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return invalid("the file name must not end with a dot or a space")
	}
	if strings.Contains(name, "..") {
		return invalid("the file name must not contain two dots in a row")
	}

	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 {
		return invalid("the file name needs an extension: %s", allowedExtensionList())
	}
	if !AllowedExtensions[strings.ToLower(name[dot:])] {
		return invalid("files of the kind %q are not allowed; use %s", name[dot:], allowedExtensionList())
	}

	stem := name
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	if reservedDeviceNames[strings.ToUpper(strings.TrimRight(stem, " "))] {
		return invalid("%q is a reserved name; choose another", stem)
	}
	return nil
}

// allowedExtensionList is the list for the message the model reads, in a fixed order.
func allowedExtensionList() string {
	return ".txt, .md, .csv, .json, .yaml, .yml or .log"
}

// describeRune names a character for the message the model reads without echoing a
// control character or a long text back.
func describeRune(r rune) string {
	switch {
	case r == '/' || r == '\\':
		return "\"" + string(r) + "\", folders are not supported"
	case unicode.IsControl(r):
		return "a control character"
	case r == utf8.RuneError:
		return "an invalid character"
	}
	return fmt.Sprintf("%q", r)
}
