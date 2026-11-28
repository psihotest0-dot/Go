package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"

	"go-monolith/internal/taskplan"
	"go-monolith/internal/taskrun"
	"go-monolith/internal/webfetch"
	"go-monolith/metrics"
)

// A research on the internet into one file is walked by the server (plan П55, phase 3, recipe Р2;
// 2026-11-28). In Telegram on 2026-11-24 the owner asked "Опитай да направиш справочник … който би
// послужил за изграждане на успешен OCR проект", then "От интернет имах предвид да,започни": the
// model answered "ще изготвя този справочник", and nothing was made. Replayed on the live backend of
// 2026-11-28 three times: no page read, no file; twice the guide was written in the chat from the
// snippets of the search, once with "Създадох справочник" and no file.
//
// Now such a message is answered by the code: the local model only reads the subject and the search
// from the request (a short call of its own, as for the reminders), the code searches, keeps the
// first pages that can be read as text (a video or a social network cannot), and proposes them;
// the person's yes starts the walk (taskrun/research.go): each page is read from this machine, its
// notes on the subject are a section of ONE file with its address. The addresses come from the
// search, not from the person: they are shown in the proposal and none is opened before the yes
// (the rule the owner approved for Р3 on 2026-11-28).

var (
	// researchProduct: what is to be made of the pages - a guide, notes, a review, a file.
	researchProduct = regexp.MustCompile(`(?i)(?:^|\P{L})(?:справочни\p{L}*|наръчни\p{L}*|ръководств\p{L}*|обзор\p{L}*|бележк\p{L}*|доклад\p{L}*|` +
		`файл\p{L}*|резюме\p{L}*|reference|handbook|guide|notes|report|file|summary)(?:\P{L}|$)`)
	// researchVerb: it is to be made, not read: "направи", "състави", "запиши".
	researchVerb = regexp.MustCompile(`(?i)(?:^|\P{L})(?:направ|създа|състав|изготв|подготв|запиш|напиш|събер|оформ|` +
		`make|create|write|compile|prepare|put\s+together)\p{L}*`)
	// researchLibrary: the library or the books are named - a question to them (books_web.go) or an add.
	researchLibrary = regexp.MustCompile(`(?i)библиотек|книгите|документите|\blibrary\b|\bbooks\b`)
)

const (
	// maxResearchFollowUpRunes: "от интернет имах предвид, започни" says only where to look; the
	// request is the message before it.
	maxResearchFollowUpRunes = 80
	// researchReadTimeout bounds the call of the model that reads the request.
	researchReadTimeout = 60 * time.Second
	// maxResearchQueryRunes and maxResearchTopicRunes bound what the model reads from the request.
	maxResearchQueryRunes = 100
	maxResearchTopicRunes = 60
)

// asksForResearch tells whether a message asks for a file made from pages of the internet: "намери в
// интернет статии за OCR и направи справочник във файл". previous is the person's message before it:
// "от интернет имах предвид, започни" after "направи справочник за OCR проект" is one too.
func asksForResearch(message, previous string) bool {
	m := strings.TrimSpace(withoutClosingNote(message))
	if m == "" || urlPattern.MatchString(m) || utf8.RuneCountInString(m) > maxKnowledgeQuestionRunes || researchLibrary.MatchString(m) {
		return false
	}
	if !webAskPlace.MatchString(m) {
		return false
	}
	if researchProduct.MatchString(m) && researchVerb.MatchString(m) {
		return true
	}
	p := strings.TrimSpace(withoutClosingNote(previous))
	return utf8.RuneCountInString(m) <= maxResearchFollowUpRunes && p != "" && !urlPattern.MatchString(p) &&
		utf8.RuneCountInString(p) <= maxKnowledgeQuestionRunes && !researchLibrary.MatchString(p) &&
		researchProduct.MatchString(p) && researchVerb.MatchString(p)
}

// previousUserMessage is the person's message before the newest one in the history (the newest is the
// message of the turn), "" when there is none.
func previousUserMessage(history []openai.ChatCompletionMessage) string {
	seen := false
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != openai.ChatMessageRoleUser {
			continue
		}
		if seen {
			return history[i].Content
		}
		seen = true
	}
	return ""
}

// researchFields are what the model reads in a request for a research.
type researchFields struct {
	Topic string `json:"topic"`
	Query string `json:"query"`
}

const researchReadPrompt = `You read a person's request to find pages on the internet and make a guide (a file of notes) from them. Fill in two fields. Answer with the JSON only.
- topic: the subject of the guide in Bulgarian, 2 to 6 words, the way a title of a file names it: "изграждане на OCR система", "стоицизъм и тревожност", "грижа за орхидеи". No verbs of the request ("намери", "направи", "опитай").
- query: what to type into a web search engine to find good free articles, tutorials and examples on the subject, at most 100 characters. In English for a technical, scientific or worldwide subject; in Bulgarian for a Bulgarian one (Bulgarian law, places or prices in Bulgaria).
The earlier messages may hold the subject when the last one only says where to look ("от интернет имах предвид").`

func researchSchema() json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"topic": map[string]any{"type": "string", "maxLength": maxResearchTopicRunes},
			"query": map[string]any{"type": "string", "maxLength": maxResearchQueryRunes},
		},
		"required":             []string{"topic", "query"},
		"additionalProperties": false,
	})
	return raw
}

// readResearch asks the local model for the subject and the search of a request; earlier are the
// person's messages before it (the oldest first).
func (o *Orchestrator) readResearch(ctx context.Context, message string, earlier []string) (researchFields, error) {
	messages := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: researchReadPrompt}}
	for _, e := range earlier {
		messages = append(messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: truncateRunes(withoutClosingNote(e), maxKnowledgeQuestionRunes)})
	}
	messages = append(messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: withoutClosingNote(message)})
	reqCtx, cancel := context.WithTimeout(ctx, researchReadTimeout)
	defer cancel()
	resp, err := o.llmClient.ChatWithJSONSchema(reqCtx, openai.ChatCompletionRequest{
		Messages: messages,
		//lint:ignore SA1019 llama-server reads max_tokens; max_completion_tokens is OpenAI's own name.
		MaxTokens:   200,
		Temperature: 0.1,
	}, researchSchema())
	if err != nil {
		return researchFields{}, err
	}
	if len(resp.Choices) == 0 {
		return researchFields{}, errors.New("the model gave no answer")
	}
	content := strings.TrimSpace(resp.Choices[0].Message.Content)
	content = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(content, "```json"), "```"), "```")
	var f researchFields
	if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &f); err != nil {
		return researchFields{}, fmt.Errorf("the answer is not the JSON asked for (%d characters): %w", len(content), err)
	}
	f.Topic = strings.Trim(strings.Join(strings.Fields(f.Topic), " "), " .,;:!?\"„“")
	f.Query = strings.Join(strings.Fields(f.Query), " ")
	return f, nil
}

// webHit is one result of a search; Text is its summary or snippet.
type webHit struct {
	Title, Link, Site, Text string
}

// webSearcher searches the internet for the code (DefaultToolRegistry) and reads a page.
type webSearcher interface {
	canResearch() bool
	searchWeb(ctx context.Context, query string) ([]webHit, error)
}

func (r *DefaultToolRegistry) canResearch() bool { return r.pages != nil }

// searchWeb is the paid search the tool web_search makes, for the code: the results in the order of
// the search.
func (r *DefaultToolRegistry) searchWeb(ctx context.Context, query string) ([]webHit, error) {
	if err := webSearchQuery(query); err != nil {
		return nil, err
	}
	result, err := r.callWebTool(ctx, "web_search", "/search", map[string]interface{}{"query": query})
	if err != nil {
		return nil, err
	}
	answer, _ := result["result"].(map[string]interface{})
	list, _ := answer["results"].([]interface{})
	var hits []webHit
	for _, item := range list {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		text := func(name string) string {
			s, _ := m[name].(string)
			return strings.Join(strings.Fields(s), " ")
		}
		hit := webHit{Title: text("title"), Link: text("url"), Site: text("site_name")}
		for _, name := range []string{"summary", "snippet", "content"} {
			if body := text(name); body != "" {
				hit.Text = cutAtWord(body, maxWebSearchTextRunes)
				break
			}
		}
		hits = append(hits, hit)
	}
	return hits, nil
}

// noTextHosts are sites whose pages give no text to a program: videos, social networks, a login.
var noTextHosts = []string{"youtube.com", "youtu.be", "vimeo.com", "tiktok.com", "facebook.com", "instagram.com",
	"x.com", "twitter.com", "linkedin.com", "pinterest.com", "reddit.com", "quora.com"}

// researchPages are the results of a search a research reads: the first ones with an address that may
// be fetched, one a site and one a title (the same article on two sites), none of a site without text
// and none whose title is in a script the person does not read, at most taskrun.MaxResearchPages. The
// part of an address after "#" is dropped: the search adds "#1" to some, and a page has no such part.
//
// On 2026-11-28 the search for "OCR project examples architecture tutorials best practices" gave,
// among its first five, the same article in Chinese on two sites, and a page with an Arabic title.
func researchPages(hits []webHit) []webHit {
	var out []webHit
	sites, titles := map[string]bool{}, map[string]bool{}
	for _, h := range hits {
		if len(out) == taskrun.MaxResearchPages {
			break
		}
		if _, err := validateFetchURL(h.Link); err != nil {
			continue
		}
		u, err := url.Parse(h.Link)
		if err != nil {
			continue
		}
		host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
		title := strings.ToLower(strings.Join(strings.Fields(h.Title), " "))
		if sites[host] || noTextHost(host) || (title != "" && titles[title]) || foreignScript(h.Title) {
			continue
		}
		sites[host] = true
		if title != "" {
			titles[title] = true
		}
		u.Fragment, u.RawFragment = "", ""
		h.Link = u.String()
		out = append(out, h)
	}
	return out
}

// foreignScript tells whether a title is mostly in a script other than Latin, Cyrillic or Greek:
// Chinese, Japanese, Korean, Arabic, Hebrew, Thai, Devanagari. Its page is no source the person reads.
func foreignScript(title string) bool {
	letters, foreign := 0, 0
	for _, r := range title {
		if !unicode.IsLetter(r) {
			continue
		}
		letters++
		if !unicode.In(r, unicode.Latin, unicode.Cyrillic, unicode.Greek) {
			foreign++
		}
	}
	return letters > 0 && foreign*10 >= letters*3
}

func noTextHost(host string) bool {
	for _, h := range noTextHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// researchAnswer is the answer of the code to a request for a research on the internet: the proposal,
// or why there is none; "" when the message is no such request or the server cannot walk it.
func (o *Orchestrator) researchAnswer(ctx context.Context, userID uuid.UUID, session, message string, history []openai.ChatCompletionMessage) string {
	if o.runPlans == nil {
		return ""
	}
	previous := previousUserMessage(history)
	if !asksForResearch(message, previous) {
		return ""
	}
	searcher, ok := o.tools.(webSearcher)
	if !ok || !searcher.canResearch() {
		return ""
	}
	var earlier []string
	if previous != "" {
		earlier = append(earlier, previous)
	}
	fields, err := o.readResearch(ctx, message, earlier)
	if err != nil || fields.Topic == "" || fields.Query == "" {
		slog.WarnContext(ctx, "the request of a research could not be read; the model answers", "err", err)
		return ""
	}
	hits, err := searcher.searchWeb(ctx, fields.Query)
	if err != nil {
		slog.WarnContext(ctx, "the search of a research failed; the model answers", "err", err)
		return ""
	}
	pages := researchPages(hits)
	if len(pages) == 0 {
		metrics.TaskRuns.WithLabelValues(taskrun.KindResearch, "no_documents").Inc()
		return fmt.Sprintf("Потърсих в интернет „%s“, но не намерих страници, които мога да прочета като текст (видео и социални мрежи не стават). "+
			"Опиши темата с други думи и пак ще потърся.", fields.Query)
	}
	steps := make([]taskplan.Step, 0, len(pages))
	for _, h := range pages {
		steps = append(steps, taskplan.Step{ID: len(steps) + 1, Text: hitName(h), Link: h.Link, About: fields.Topic, Query: fields.Query, Status: taskplan.StepPending})
	}
	if _, err := o.runPlans.Propose(ctx, userID, session, taskrun.KindResearch, researchGoal(fields.Topic), steps); err != nil {
		slog.WarnContext(ctx, "could not store the proposed research; the model answers", "err", err)
		return ""
	}
	metrics.TaskRuns.WithLabelValues(taskrun.KindResearch, "proposed").Inc()
	// The "да" that follows is for the research, not for a bill or a reminder that waited.
	o.dropOtherProposals(ctx, userID, session, proposalTask)
	return researchProposalText(fields, pages)
}

// hitName is how a page found by the search is named in a list: its title, else its host.
func hitName(h webHit) string {
	if h.Title != "" {
		return truncateRunes(h.Title, 90)
	}
	return hostOf(h.Link)
}

// researchGoal is the goal of a research, the way the report and the status answer name it.
func researchGoal(topic string) string {
	return fmt.Sprintf("справочник „%s“ от страници в интернет", topic)
}

// researchProposalText is the question before the start of a research.
func researchProposalText(f researchFields, pages []webHit) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Потърсих в интернет „%s“. Мога да прочета тези %d страници и да направя един файл „%s“ — за всяка страница раздел с бележките ѝ по темата „%s“ и линка ѝ:\n",
		f.Query, len(pages), taskrun.FileName(f.Topic, "справочник", 0), f.Topic)
	for _, h := range pages {
		site := h.Site
		if site == "" {
			site = hostOf(h.Link)
		}
		b.WriteString("• " + hitName(h) + " — " + site + "\n")
	}
	fmt.Fprintf(&b, "\nСтраниците ги отварям чак след твоето „да“ (адресите са от търсенето, не от теб). Всеки раздел е писан само от текста на своята страница "+
		"и числата се проверяват срещу него; страница, която не се отваря или няма нищо по темата, пропускам. Ще отнеме около %s и върви на заден план. "+
		"Нищо в папката не се заменя.\n\nДа започна ли?", estimateSeconds(len(pages)*taskrun.SecondsPerPage))
	return b.String()
}

// ReadPage reads a page of a research the server walks (taskrun.PageReader): from this machine, else
// its copy in the Wayback Machine. The address is one the person said yes to: the proposal showed it.
func (r *DefaultToolRegistry) ReadPage(ctx context.Context, link string) (string, string, error) {
	if r.pages == nil {
		return "", "", errors.New("четенето на страници е изключено")
	}
	if _, err := validateFetchURL(link); err != nil {
		return "", "", errors.New(linkReasonWords(string(webfetch.ReasonNotPublic)))
	}
	title, text, _, err := r.pages.FetchText(ctx, link)
	if err == nil {
		metrics.WebToolRequests.WithLabelValues(researchPageTool, "local").Inc()
		return title, text, nil
	}
	reason := localFailure(err)
	metrics.WebToolRequests.WithLabelValues(researchPageTool, "local_"+reason).Inc()
	if title, text, _, _, ok := r.archivedPage(ctx, researchPageTool, link, reason); ok {
		return title, text, nil
	}
	slog.InfoContext(ctx, "a page of a research could not be read", "host", hostOf(link), "reason", reason)
	if reason == "empty" {
		return "", "", errors.New("страницата няма текст, който да се прочете (може да е направена с JavaScript)")
	}
	return "", "", errors.New(linkReasonWords(reason))
}

// researchPageTool is the name of the reading of a page of a research in the counters and the log.
const researchPageTool = "research_page"
