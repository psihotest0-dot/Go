package agent

import (
	"context"
	"io"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
	"go-monolith/internal/db"
	"go-monolith/internal/webfetch"
)

// dbAdder puts a row in the library of the test database for every document, as the real adder does.
type dbAdder struct {
	db    *db.Database
	added int
}

func (a *dbAdder) Add(ctx context.Context, userID uuid.UUID, name string, src io.Reader) (api.AddedFile, error) {
	body, _ := io.ReadAll(src)
	file, err := a.db.Queries.CreateIngestedFile(ctx, db.CreateIngestedFileParams{
		UserID: userID, FileName: name, FileHash: "h-" + uuid.NewString(), MimeType: "text/html",
		FileSize: int64(len(body)), Status: "pending", LocalPath: "storage/none",
	})
	if err != nil {
		return api.AddedFile{}, err
	}
	a.added++
	return api.AddedFile{ID: file.ID, Name: file.FileName, Size: file.FileSize}, nil
}

// A page changes between two downloads: every fetch gives other bytes.
func changingPage() *fakeFetcher {
	return &fakeFetcher{docs: map[string]*webfetch.Document{}}
}

func TestTheLinkOfADocumentIsKeptAndFoundAgain(t *testing.T) {
	e := newToolEnv(t)
	ctx := context.Background()
	file, err := e.db.Queries.CreateIngestedFile(ctx, db.CreateIngestedFileParams{
		UserID: e.userID, FileName: "Paper Trading.html", FileHash: "h-" + uuid.NewString(),
		MimeType: "text/html", FileSize: 10, Status: "completed", LocalPath: "x",
	})
	require.NoError(t, err)

	e.reg.rememberSource(ctx, e.userID, file.ID, "https://docs.alpaca.markets/docs/paper-trading/")
	found, ok := e.reg.librarySource(ctx, e.userID, "http://docs.alpaca.markets/docs/paper-trading#orders")
	require.True(t, ok, "the same link without its scheme, slash and fragment")
	assert.Equal(t, file.ID, found.ID)

	other, err := e.db.Queries.CreateIngestedFile(ctx, db.CreateIngestedFileParams{
		UserID: e.userID, FileName: "other.html", FileHash: "h-" + uuid.NewString(),
		MimeType: "text/html", FileSize: 10, Status: "completed", LocalPath: "x",
	})
	require.NoError(t, err)
	e.reg.rememberSource(ctx, e.userID, file.ID, "https://elsewhere.example/a")
	found, _ = e.reg.librarySource(ctx, e.userID, "https://docs.alpaca.markets/docs/paper-trading")
	assert.Equal(t, file.ID, found.ID, "the first link of a document stays")
	_, ok = e.reg.librarySource(ctx, e.userID, "https://elsewhere.example/a")
	assert.False(t, ok)
	_ = other

	_, ok = e.reg.librarySource(ctx, uuid.New(), "https://docs.alpaca.markets/docs/paper-trading")
	assert.False(t, ok, "another person's library")

	_, err = e.db.SqlDB.ExecContext(ctx, `UPDATE ingested_files SET status = 'failed' WHERE id = $1`, file.ID)
	require.NoError(t, err)
	_, ok = e.reg.librarySource(ctx, e.userID, "https://docs.alpaca.markets/docs/paper-trading")
	assert.False(t, ok, "a document whose processing failed does not count: the link may be tried again")
}

func TestAPageAddedTwiceIsFetchedAndAddedOnce(t *testing.T) {
	e := newToolEnv(t)
	f := changingPage()
	adder := &dbAdder{db: e.db}
	e.reg.WithLibraryAdd(f, adder)
	const link = "https://docs.alpaca.markets/docs/paper-trading"
	ctx, _ := libraryAddCtx(t, "добави в библиотеката: "+link)
	ctx = context.WithValue(ctx, api.ParsedUserIDKey, e.userID)

	first := rowsOf(t, call(t, e.reg, ctx, link))
	assert.Equal(t, addQueued, first[0].Status)

	second := rowsOf(t, call(t, e.reg, ctx, link))
	assert.Equal(t, addDuplicate, second[0].Status)
	assert.Equal(t, first[0].File, second[0].File)
	assert.Equal(t, int32(1), f.calls.Load(), "the second time the link is not fetched")
	assert.Equal(t, 1, adder.added)

	// The task over the links and the link check see it too.
	id, name, err := e.reg.AddLink(context.Background(), e.userID, link+"/")
	require.NoError(t, err)
	assert.Equal(t, first[0].File, name)
	assert.NotEqual(t, uuid.Nil, id)
	probes := e.reg.probeLinks(context.Background(), e.userID, []string{link})
	assert.Equal(t, first[0].File, probes[0].InLibrary)
	assert.Equal(t, id, probes[0].InLibraryID)
	assert.Equal(t, int32(1), f.calls.Load(), "neither fetches it")
	assert.Equal(t, 1, adder.added)
}

func TestALinkAddedByTheTaskIsKeptForTheNextAdd(t *testing.T) {
	e := newToolEnv(t)
	f := changingPage()
	adder := &dbAdder{db: e.db}
	e.reg.WithLibraryAdd(f, adder)
	const link = "https://arxiv.org/pdf/2603.11838"

	_, _, err := e.reg.AddLink(context.Background(), e.userID, link)
	require.NoError(t, err)
	_, _, err = e.reg.AddLink(context.Background(), e.userID, link)
	require.NoError(t, err)

	assert.Equal(t, int32(1), f.calls.Load())
	assert.Equal(t, 1, adder.added)
}
