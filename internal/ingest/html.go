package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// htmlMaxToken bounds the memory the tokenizer may hold for one token. A tag
// carries its attributes, so a page with a base64 image inlined in an <img>
// tag is one large token; 32 MiB is far above any real page.
const htmlMaxToken = 32 << 20

// HTMLExtractor reads web pages: the text of the article, without the menus, the footer and the
// side bars of the site (2026-11-28). The live check of that day read an article of the New York
// Fed into notes whose first part was the bank's own menu and mission. EPUB chapters are read
// whole (epub.go): a <header> there is the chapter's title, not a menu.
type HTMLExtractor struct{}

func NewHTMLExtractor() *HTMLExtractor {
	return &HTMLExtractor{}
}

func (e *HTMLExtractor) Name() string {
	return "HTMLExtractor"
}

func (e *HTMLExtractor) Supports(mimeType, ext string) bool {
	if strings.HasPrefix(mimeType, "text/html") {
		return true
	}
	return strings.ToLower(ext) == ".html" || strings.ToLower(ext) == ".htm"
}

// Extract is the text of the article of a page (see the type).
func (e *HTMLExtractor) Extract(ctx context.Context, r io.Reader) (string, error) {
	page := &webPage{all: newTextBuilder(), main: newTextBuilder()}
	if err := writeHTMLText(ctx, r, page.all, page); err != nil {
		return "", err
	}
	return page.article(), nil
}

// PageText is all the visible text of a page, menus and footer included: what the check for a
// page with only an abstract counts (webfetch), whose threshold was set on it.
func PageText(ctx context.Context, r io.Reader) (string, error) {
	tb := newTextBuilder()
	if err := writeHTMLText(ctx, r, tb, nil); err != nil {
		return "", err
	}
	return strings.TrimSpace(tb.String()), nil
}

func isBlockElement(tag string) bool {
	switch tag {
	case "p", "div", "h1", "h2", "h3", "h4", "h5", "h6", "li", "tr", "article", "section", "blockquote":
		return true
	}
	return false
}

func isSkippedElement(tag string) bool {
	switch tag {
	case "script", "style", "noscript", "template":
		return true
	}
	return false
}

var (
	// chromeTags are the elements of a web page that hold the site, not the article.
	chromeTags = map[string]bool{
		"nav": true, "footer": true, "form": true, "button": true, "select": true,
		"dialog": true, "svg": true, "iframe": true, "menu": true,
	}
	// chromeRoles are the ARIA roles of the same.
	chromeRoles = map[string]bool{
		"navigation": true, "banner": true, "contentinfo": true, "search": true, "complementary": true,
		"menu": true, "menubar": true, "dialog": true, "alertdialog": true,
	}
	// chromeNames: a class or an id of a menu, a footer, a side bar, a cookie notice, buttons to share.
	// sidebarName: a side bar, which is a side note of the text inside an article.
	sidebarName = regexp.MustCompile(`(?i)(?:^|[-_])sidebar(?:[-_]|$)`)
	chromeNames = regexp.MustCompile(`(?i)(?:^|[-_])(?:nav|navbar|navigation|menu|menus|breadcrumbs?|footer|sidebar|cookies?|consent|share|sharing|social|subscribe|newsletter|skip-link|skiplink|masthead|toc)(?:[-_]|$)`)
)

// minArticleRunes and minArticleShare: a <main> or an <article> is the page's article when it holds
// this much text, or this share of the text outside the chrome; else all that text is taken (a
// small <article> may be one teaser in a list of them).
const (
	minArticleRunes = 2000
	minArticleShare = 0.4
)

// webPage is what a page was read into: the text outside the chrome, the text of its main region,
// and its title.
type webPage struct {
	all, main *textBuilder
	title     string
}

// article is the text of the page: its title and its main region when that is the article, else
// all its text outside the chrome.
func (p *webPage) article() string {
	all := strings.TrimSpace(p.all.String())
	main := strings.TrimSpace(p.main.String())
	n := utf8.RuneCountInString(main)
	if n == 0 || (n < minArticleRunes && float64(n) < minArticleShare*float64(utf8.RuneCountInString(all))) {
		return all
	}
	if p.title != "" && !strings.Contains(main[:min(len(main), 2000)], p.title) {
		return p.title + "\n\n" + main
	}
	return main
}

// isChrome tells whether an element holds the site and not the article: its tag, its role, its
// class or id, or that it is hidden. A <header> and an <aside> are chrome outside the main region
// only: inside an article the one holds its title and the other a side note of the text (the
// sidebars of an essay on martinfowler.com, 15 paragraphs of it, were lost when <aside> was chrome
// everywhere); for the same reason a "sidebar" class inside the main region is kept.
func isChrome(tag string, attrs map[string]string, inMain bool) bool {
	if chromeTags[tag] || (!inMain && (tag == "header" || tag == "aside")) {
		return true
	}
	if chromeRoles[strings.ToLower(attrs["role"])] {
		return true
	}
	if _, hidden := attrs["hidden"]; hidden || strings.EqualFold(attrs["aria-hidden"], "true") {
		return true
	}
	switch tag {
	case "div", "section", "ul", "ol", "header", "span", "table":
		for _, name := range strings.Fields(attrs["class"] + " " + attrs["id"]) {
			if chromeNames.MatchString(name) && !(inMain && sidebarName.MatchString(name)) {
				return true
			}
		}
	}
	return false
}

// isMainRegion: the element that holds the article of a page.
func isMainRegion(tag string, attrs map[string]string) bool {
	return tag == "main" || tag == "article" || strings.EqualFold(attrs["role"], "main")
}

// tagAttrs are the attributes of the tag the tokenizer is on that tell chrome and the main region.
func tagAttrs(z *html.Tokenizer, hasAttr bool) map[string]string {
	attrs := map[string]string{}
	for hasAttr {
		var key, val []byte
		key, val, hasAttr = z.TagAttr()
		switch k := string(key); k {
		case "role", "class", "id", "hidden", "aria-hidden":
			attrs[k] = string(val)
		}
	}
	return attrs
}

// htmlText is where writeHTMLText writes: always tb; with a web page, the text of its main region
// also into page.main.
type htmlText struct {
	tb     *textBuilder
	page   *webPage
	inMain bool
}

func (h *htmlText) write(s string) error {
	if err := h.tb.WriteString(s); err != nil {
		return err
	}
	if h.inMain {
		return h.page.main.WriteString(s)
	}
	return nil
}

func (h *htmlText) newline() error {
	if err := h.tb.Newline(); err != nil {
		return err
	}
	if h.inMain {
		return h.page.main.Newline()
	}
	return nil
}

func (h *htmlText) paragraph() error {
	if err := h.tb.Paragraph(); err != nil {
		return err
	}
	if h.inMain {
		return h.page.main.Paragraph()
	}
	return nil
}

// writeHTMLText decodes an HTML or XHTML document (any encoding newHTMLReader
// understands) and appends its visible text to tb, with a blank line between
// block elements. With page (a web page, not a chapter of a book) the chrome of the
// site is left out, the text of the main region also goes to page.main, and the
// <title> to page.title.
//
// It reads tokens instead of building a tree: the tree of a 100 MB page of
// small tags needs gigabytes, the token stream needs a few kilobytes. A block
// element breaks the text where it starts and where it ends, so a document
// that leaves out the closing </p> still gets its paragraphs apart.
func writeHTMLText(ctx context.Context, r io.Reader, tb *textBuilder, page *webPage) error {
	decoded, err := newHTMLReader(r)
	if err != nil {
		return err
	}
	z := html.NewTokenizer(decoded)
	z.SetMaxBuf(htmlMaxToken)
	out := &htmlText{tb: tb, page: page}

	var skipTag string // the element whose content is being skipped (script, style, chrome)
	skipDepth := 0
	var mainTag string // the main region of a web page being read
	mainDepth := 0
	inTitle := false

	for tokens := 0; ; tokens++ {
		if tokens%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			err := z.Err()
			switch {
			case errors.Is(err, io.EOF):
				return nil
			case errors.Is(err, html.ErrBufferExceeded):
				return fmt.Errorf("%w: a single HTML token is larger than %d MiB", ErrInputTooLarge, htmlMaxToken>>20)
			}
			return err

		case html.TextToken:
			if inTitle && page != nil && page.title == "" {
				page.title = strings.Join(strings.Fields(string(z.Text())), " ")
			}
			if skipDepth > 0 {
				continue
			}
			if text := strings.TrimSpace(string(z.Text())); text != "" {
				if err := out.write(text + " "); err != nil {
					return err
				}
			}

		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			tag := string(name)
			if tag == "title" && tt == html.StartTagToken {
				inTitle = true
			}
			if skipDepth > 0 {
				if tag == skipTag && tt == html.StartTagToken {
					skipDepth++
				}
				continue
			}
			var attrs map[string]string
			if page != nil {
				attrs = tagAttrs(z, hasAttr)
			}
			switch {
			case tt == html.SelfClosingTagToken && (isSkippedElement(tag) || page != nil && chromeTags[tag]):
				// Nothing inside, nothing to skip.
			case isSkippedElement(tag):
				skipTag, skipDepth = tag, 1
			case page != nil && tt == html.StartTagToken && isChrome(tag, attrs, mainDepth > 0):
				skipTag, skipDepth = tag, 1
			case tag == "br":
				if err := out.newline(); err != nil {
					return err
				}
			default:
				if page != nil && tt == html.StartTagToken {
					switch {
					case mainDepth > 0 && tag == mainTag:
						mainDepth++
					case mainDepth == 0 && isMainRegion(tag, attrs):
						mainTag, mainDepth = tag, 1
						out.inMain = true
					}
				}
				if isBlockElement(tag) {
					if err := out.paragraph(); err != nil {
						return err
					}
				}
			}

		case html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if tag == "title" {
				inTitle = false
			}
			if skipDepth > 0 {
				if tag == skipTag {
					skipDepth--
				}
				continue
			}
			if isBlockElement(tag) {
				if err := out.paragraph(); err != nil {
					return err
				}
			}
			if mainDepth > 0 && tag == mainTag {
				mainDepth--
				out.inMain = mainDepth > 0
			}
		}
	}
}
