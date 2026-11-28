package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func extractPage(t *testing.T, page string) string {
	t.Helper()
	text, err := NewHTMLExtractor().Extract(context.Background(), strings.NewReader(page))
	require.NoError(t, err)
	return text
}

// The article of 2026-11-28 had the site's menu and mission before it and an archive by months
// after it; the notes made of it started with the bank's mission.
var siteWithArticle = `<html><head><title>The Disappearing Overnight Drift - Liberty Street Economics</title></head><body>
<header class="site-header"><a href="/">Museum &amp; Visits</a> <a href="/">Careers</a></header>
<nav><ul><li>About the New York Fed</li><li>Governance</li></ul></nav>
<div class="mega-menu"><p>At the New York Fed, our mission is to make the U.S. economy stronger.</p></div>
<div role="navigation">Data &amp; Statistics</div>
<main>
<article>
<header><h1>The Disappearing Overnight Drift</h1><p>Nina Boyarchenko, Lars C. Larsen, and Paul Whelan</p></header>
<p>` + articleBody + `</p>
<aside class="sidebar"><p>How big is the drift? A side note of the article.</p></aside>
<div class="sidebar"><p>A second side note, in a div.</p></div>
<div class="share-buttons">Share on X</div>
<div role="search">Search the blog</div>
<span aria-hidden="true">Hidden icon text</span>
</article>
</main>
<aside class="widget"><p>Archive: October 2012 September 2012</p></aside>
<footer><p>Terms of Use Privacy Site Map</p></footer>
<div class="cookie-banner" hidden>We use cookies</div>
</body></html>`

var articleBody = strings.Repeat("In a 2021 Liberty Street Economics post we showed that returns accrue overnight. ", 40)

func TestAWebPageGivesItsArticleWithoutTheSite(t *testing.T) {
	text := extractPage(t, siteWithArticle)

	assert.True(t, strings.HasPrefix(text, "The Disappearing Overnight Drift - Liberty Street Economics"), "the title of the page first:\n%s", text[:200])
	assert.Contains(t, text, "Nina Boyarchenko", "the header of the article is its own")
	assert.Contains(t, text, "returns accrue overnight")
	assert.Contains(t, text, "How big is the drift?", "a side note inside the article is the article's")
	assert.Contains(t, text, "A second side note, in a div.")
	for _, chrome := range []string{"Museum", "About the New York Fed", "our mission", "Data & Statistics", "Archive", "Terms of Use", "cookies", "Share on X", "Search the blog", "Hidden icon text"} {
		assert.NotContains(t, text, chrome)
	}
}

func TestAPageWithoutAMainRegionLosesOnlyItsChrome(t *testing.T) {
	page := `<html><body><nav>Home Essays</nav><div id="toc">Contents</div>
<table><tr><td><p>July 2023</p><p>If you collected lists of techniques for doing great work...</p></td></tr></table>
<footer>Copyright</footer></body></html>`
	text := extractPage(t, page)
	assert.Contains(t, text, "July 2023")
	assert.Contains(t, text, "techniques for doing great work")
	assert.NotContains(t, text, "Home Essays")
	assert.NotContains(t, text, "Contents")
	assert.NotContains(t, text, "Copyright")
}

// A small <article> in a long page is one teaser of a list, not the page's article.
func TestASmallArticleInALongPageIsNotTakenForThePage(t *testing.T) {
	page := `<html><body><div><p>` + articleBody + `</p></div><article><p>A teaser of another post.</p></article></body></html>`
	text := extractPage(t, page)
	assert.Contains(t, text, "returns accrue overnight")
	assert.Contains(t, text, "A teaser of another post.")
}

func TestTheWholePageTextKeepsTheSite(t *testing.T) {
	text, err := PageText(context.Background(), strings.NewReader(siteWithArticle))
	require.NoError(t, err)
	for _, s := range []string{"Museum", "our mission", "returns accrue overnight", "Terms of Use"} {
		assert.Contains(t, text, s, "the check for an abstract counts all of it")
	}
}

// A chapter of a book keeps its <header>: there it is the title of the chapter.
func TestAChapterOfABookKeepsItsHeaderAndAside(t *testing.T) {
	tb := newTextBuilder()
	chapter := `<html><body><header><h1>Chapter One</h1></header><aside><p>An epigraph.</p></aside><nav>Contents</nav><p>It was a dark night.</p></body></html>`
	require.NoError(t, writeHTMLText(context.Background(), strings.NewReader(chapter), tb, nil))
	text := tb.String()
	assert.Contains(t, text, "Chapter One")
	assert.Contains(t, text, "An epigraph.")
	assert.Contains(t, text, "It was a dark night.")
}

func TestNestedChromeAndNestedMainAreCountedRight(t *testing.T) {
	page := `<html><body><div class="nav"><div><div>Menu item</div></div><p>still menu</p></div>
<main><div><main><p>` + articleBody + `</p></main></div><p>After the inner main.</p></main>
<p>Outside the main.</p></body></html>`
	text := extractPage(t, page)
	assert.NotContains(t, text, "Menu item")
	assert.NotContains(t, text, "still menu", "the nested divs of a menu are all skipped")
	assert.Contains(t, text, "After the inner main.", "the main region ends where its own tag ends")
	assert.NotContains(t, text, "Outside the main.")
}
