package userfiles

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go-monolith/internal/api"
)

// fakeLibrary records what it was given and answers as it is told.
type fakeLibrary struct {
	user      uuid.UUID
	name      string
	text      string
	duplicate bool
	err       error
}

func (f *fakeLibrary) Add(_ context.Context, userID uuid.UUID, name string, src io.Reader) (api.AddedFile, error) {
	if f.err != nil {
		return api.AddedFile{}, f.err
	}
	b, err := io.ReadAll(src)
	if err != nil {
		return api.AddedFile{}, err
	}
	f.user, f.name, f.text = userID, name, string(b)
	return api.AddedFile{ID: uuid.MustParse("11111111-2222-3333-4444-555555555555"), Name: name, Duplicate: f.duplicate, Size: int64(len(b))}, nil
}

func TestAFileOfTheFolderGoesIntoTheLibrary(t *testing.T) {
	s, _ := newStore(t, Limits{})
	lib := &fakeLibrary{}
	s.WithLibrary(lib)
	_, err := s.Create(alice, "статия.md", "# Заглавие\n\nцелият текст")
	require.NoError(t, err)

	rec := serve(t, s, alice, http.MethodPost, "/workspace/files/"+url.PathEscape("статия.md")+"/library")

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var body libraryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "статия.md", body.FileName)
	assert.False(t, body.Duplicate)
	assert.Equal(t, int64(len("# Заглавие\n\nцелият текст")), body.Size)
	assert.Equal(t, alice, lib.user)
	assert.Equal(t, "статия.md", lib.name)
	assert.Equal(t, "# Заглавие\n\nцелият текст", lib.text, "the whole text of the file")
	text, _, err := s.Text(alice, "статия.md")
	require.NoError(t, err)
	assert.Equal(t, "# Заглавие\n\nцелият текст", text, "the file stays in the folder")
}

func TestAFileTheLibraryHasAlreadyIsSaidSo(t *testing.T) {
	s, _ := newStore(t, Limits{})
	s.WithLibrary(&fakeLibrary{duplicate: true})
	_, err := s.Create(alice, "a.md", "текст")
	require.NoError(t, err)

	rec := serve(t, s, alice, http.MethodPost, "/workspace/files/a.md/library")

	require.Equal(t, http.StatusOK, rec.Code)
	var body libraryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.True(t, body.Duplicate)
}

func TestTheLibraryRouteRefuses(t *testing.T) {
	s, _ := newStore(t, Limits{})
	_, err := s.Create(alice, "a.md", "текст")
	require.NoError(t, err)

	rec := serve(t, s, alice, http.MethodPost, "/workspace/files/a.md/library")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "without the library")

	lib := &fakeLibrary{}
	s.WithLibrary(lib)
	assert.Equal(t, http.StatusNotFound, serve(t, s, alice, http.MethodPost, "/workspace/files/none.md/library").Code)
	assert.Equal(t, http.StatusNotFound, serve(t, s, bob, http.MethodPost, "/workspace/files/a.md/library").Code, "another user's file")
	assert.Equal(t, http.StatusBadRequest, serve(t, s, alice, http.MethodPost, "/workspace/files/a.exe/library").Code, "a bad name")
	assert.Equal(t, http.StatusUnauthorized, serve(t, s, uuid.Nil, http.MethodPost, "/workspace/files/a.md/library").Code)
	assert.Empty(t, lib.name, "nothing reached the library")

	for err, want := range map[error]int{
		api.ErrAddTooLarge:         http.StatusRequestEntityTooLarge,
		api.ErrAddEmpty:            http.StatusBadRequest,
		errors.New("disk is full"): http.StatusInternalServerError,
	} {
		s.WithLibrary(&fakeLibrary{err: err})
		assert.Equal(t, want, serve(t, s, alice, http.MethodPost, "/workspace/files/a.md/library").Code, err.Error())
	}
}
