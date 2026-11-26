package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/userfiles"
	"go-monolith/internal/webfetch"
)

// save_page: the whole text of a page or a PDF into a file of the user's folder, word for word,
// taken by the server (the owner, 2026-11-23: "кодът сам да запише целия извлечен текст във
// файл"). With write_file the model writes the file itself, from the part of the page that fits
// in the turn (about 4 000 tokens) - a long article came out as its summary (2026-11-21). Here the
// code fetches the page (from this machine, the paid API when that gives nothing, as web_fetch
// does), and the file is the page: a title, the address, the day, and all of its text.
//
// The text comes from the web, so the file is a proposal the user confirms, as every write after
// text from outside is. It may be as large as a saved page (userfiles.MaxPageBytes, within the
// room of the folder); the model is not given the text, only what was proposed.
const savePageTool = "save_page"

// savePageDefinition is the tool in the group of the file tools.
func savePageDefinition() openai.Tool {
	return openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
		Name: savePageTool,
		Description: "Save the WHOLE text of a web page or a PDF at a link into a file of the user's files folder, word for word. " +
			"The server downloads it and writes all of it - you do not write the text. Use it when the user wants the whole article, page " +
			"or document kept as a file ('запиши цялата статия във файл', 'свали страницата', 'save this page'). For your own summary, notes " +
			"or a part of a text, use " + writeFileTool + ". The user confirms the file before it is written.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":  map[string]any{"type": "string", "description": "The link of the page or the PDF."},
				"name": map[string]any{"type": "string", "description": "File name, e.g. article.md. Leave it out to name the file after the title of the page."},
			},
			"required": []string{"url"},
		},
	}}
}

var (
	pageLink     = regexp.MustCompile(`(?i)https?://\S+`)
	pageSaveVerb = regexp.MustCompile(`(?i)(запиш|запаз|свал|съхран|\bsave\b|download|\bkeep\b|\bstore\b)`)
	pageAsFile   = regexp.MustCompile(`(?i)(файл|\bfile\b|\.(md|txt)\b)`)
	pageWhole    = regexp.MustCompile(`(?i)(цял(ата|ия|ото|ост)?\b|целия|целият|\bwhole\b|\bentire\b|\bfull\b|текста на|the text of)`)
	// pageOwnWords: the person wants the model's own text about the page, which write_file is for.
	pageOwnWords = regexp.MustCompile(`(?i)(резюм|обобщ|бележк|накратко|кратк|основн|ключов|преразка|превед|summar|\bnotes?\b|key points|translat|библиотек|library)`)
)

// asksToSavePage tells whether a message asks for the whole page behind a link as a file. Left to
// itself the model did that 3 times in 10 (measured 2026-11-23, go-monolith-backups\measurements\
// savepage_2026-11-23): it read the page and wrote a text of its own with write_file ("Готово,
// записах статията"), or put the page into the library for "свали страницата във файл". Such a
// message has a link, a word for saving, and a word for a file or for the whole text; a summary,
// notes, a translation or the library are something else.
func asksToSavePage(message string) bool {
	if !pageLink.MatchString(message) {
		return false
	}
	text := pageLink.ReplaceAllString(message, " ")
	if pageOwnWords.MatchString(text) {
		return false
	}
	asFile := pageAsFile.MatchString(text)
	return (pageSaveVerb.MatchString(text) && (asFile || pageWhole.MatchString(text))) || (asFile && pageWhole.MatchString(text))
}

// pageForFile is the text of a page as the file holds it.
func pageForFile(title, source, text string, day time.Time) string {
	var b strings.Builder
	if title != "" {
		b.WriteString("# " + title + "\n\n")
	}
	b.WriteString("Източник: " + source + "\n")
	b.WriteString("Свалено: " + day.Format("2006-01-02") + "\n\n")
	b.WriteString(strings.TrimSpace(text))
	b.WriteString("\n")
	return b.String()
}

// pageFileName is the name of the file of a page: the one the model gave (with .md when it has
// no extension), or one made from the title.
func pageFileName(given, title string) string {
	if given = strings.TrimSpace(given); given != "" {
		if !strings.Contains(given, ".") {
			given += ".md"
		}
		return given
	}
	var b strings.Builder
	space := false
	for _, r := range title {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteRune(r)
			space = false
		case r == '-' || r == '_':
			b.WriteRune(r)
			space = false
		default:
			space = true
		}
	}
	stem := strings.Trim(b.String(), " -_")
	if limit := userfiles.MaxNameRunes - len(".md") - len(" (99)"); utf8.RuneCountInString(stem) > limit {
		stem = strings.TrimRight(string([]rune(stem)[:limit]), " -_")
	}
	if stem == "" || !(unicode.IsLetter([]rune(stem)[0]) || unicode.IsDigit([]rune(stem)[0])) {
		stem = "страница"
	}
	return stem + ".md"
}

// freeName is name, or name with " (2)", " (3)"... before its extension when it is taken.
func (r *DefaultToolRegistry) freeName(userID uuid.UUID, name string) string {
	stem, ext := name, ""
	if dot := strings.LastIndexByte(name, '.'); dot > 0 {
		stem, ext = name[:dot], name[dot:]
	}
	candidate := name
	for i := 2; i < 100; i++ {
		if _, err := r.files.Stat(userID, candidate); errors.Is(err, userfiles.ErrNotFound) {
			return candidate
		}
		candidate = fmt.Sprintf("%s (%d)%s", stem, i, ext)
	}
	return candidate
}

// savePageClock is the clock of the day written into a saved page (a test sets it).
var savePageClock = time.Now

func (r *DefaultToolRegistry) executeSavePage(ctx context.Context, userID uuid.UUID, args map[string]interface{}) (interface{}, error) {
	if err := webAfterFilesError(ctx, savePageTool); err != nil {
		return nil, err
	}
	rawURL, ok := stringArg(args, "url")
	if !ok {
		return nil, missingArgError(savePageTool, "url", `{"url": "<page url>", "name": "article.md"}`)
	}
	pageURL, err := validateFetchURL(rawURL)
	if err != nil {
		fileOp("save_page", "refused")
		return nil, &toolArgumentError{tool: savePageTool, reason: err.Error()}
	}
	given, _ := stringArg(args, "name")
	if given != "" {
		if err := userfiles.ValidateName(pageFileName(given, "")); err != nil {
			fileOp("save_page", "refused")
			return nil, r.fileToolError(savePageTool, err)
		}
	}

	title, text, final, archived, err := r.pageText(ctx, pageURL)
	if err != nil {
		fileOp("save_page", "unread")
		return nil, err
	}
	turn := turnFrom(ctx)
	turn.markExternal(sourceWebFetch)
	if final == "" {
		final = pageURL
	}
	name := r.freeName(userID, pageFileName(given, title))
	content := pageForFile(title, final, text, savePageClock())

	p, err := r.proposals.ProposePage(ctx, userID, turn.session(), name, content, final, turn.externalSources())
	if err != nil {
		fileOp("save_page", "refused")
		if errors.Is(err, userfiles.ErrExists) {
			return nil, &toolArgumentError{tool: savePageTool, reason: "a file with this name exists; give another name"}
		}
		return nil, proposalError(savePageTool, err)
	}
	turn.addProposal(p)
	fileOp("save_page", "proposed")
	result := pendingApprovalResult(p)
	result["file_name"] = name
	result["characters"] = utf8.RuneCountInString(text)
	result["message"] = fmt.Sprintf("NOT saved yet. The whole text of the page (%d characters, title %q) waits for the user to confirm the file %q. "+
		"Tell the user briefly that the page is ready to be saved and that they can confirm or reject it. Do not say that it is saved, "+
		"and do not describe what the page says: you have not read it.", utf8.RuneCountInString(text), title, name)
	if archived != "" {
		result["archived_copy"] = archived
	}
	return result, nil
}

// pageText is the title, the text and the address of a page: from this machine first, from the
// paid API when that gives nothing (as web_fetch), and from its copy in the Wayback Machine when
// neither can read it (wayback.go); archived is then what the model is to be told of the copy.
func (r *DefaultToolRegistry) pageText(ctx context.Context, pageURL string) (title, text, final, archived string, err error) {
	localReason := ""
	if r.pages != nil {
		title, text, final, err = r.pages.FetchText(ctx, pageURL)
		if err == nil {
			return title, text, final, "", nil
		}
		localReason = localFailure(err)
		slog.InfoContext(ctx, "the local fetch of save_page gave nothing", "reason", localReason, "host", hostOf(pageURL), "error", err)
	}
	// As web_fetch: after a 404 the copy comes before the paid API.
	archiveTried := localReason == string(webfetch.ReasonNotFound)
	if archiveTried {
		if title, text, final, note, ok := r.archivedPage(ctx, savePageTool, pageURL, localReason); ok {
			return title, text, final, note, nil
		}
	}
	result, err := r.callWebTool(ctx, savePageTool, "/fetch", map[string]interface{}{"url": pageURL})
	if err == nil {
		inner, _ := result["result"].(map[string]interface{})
		content, _ := inner["content"].(string)
		heading, _ := inner["title"].(string)
		if utf8.RuneCountInString(strings.TrimSpace(content)) >= webFetchMinRunes {
			return heading, content, pageURL, "", nil
		}
		err = errPageEmpty
		localReason = "empty"
	} else if !archiveTried {
		if title, text, final, note, ok := r.archivedPage(ctx, savePageTool, pageURL, localReason); ok {
			return title, text, final, note, nil
		}
	}
	if localReason == "" {
		return "", "", "", "", err
	}
	e := localFetchError(localReason)
	var argErr *toolArgumentError
	if errors.As(e, &argErr) {
		argErr.tool = savePageTool
	}
	return "", "", "", "", e
}
