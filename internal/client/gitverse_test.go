package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/dnonakolesax/goreleaser/v2/internal/artifact"
	"github.com/dnonakolesax/goreleaser/v2/internal/testctx"
	"github.com/dnonakolesax/goreleaser/v2/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestClientNewGitVerse(t *testing.T) {
	t.Parallel()
	ctx := testctx.WrapWithCfg(t.Context(), config.Project{
		GitVerseURLs: config.GitVerseURLs{
			API: fakeGitVerse(t, nil).URL,
		},
	}, testctx.GitVerseTokenType)
	client, err := New(ctx)
	require.NoError(t, err)
	require.IsType(t, &gitVerseClient{}, client)
}

func TestGitVerseCreateRelease(t *testing.T) {
	t.Parallel()
	srv := fakeGitVerse(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/releases/tags/v1.0.0":
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/releases":
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "v1.0.0", body["tag_name"])
			require.Equal(t, "v1.0.0", body["name"])
			require.Equal(t, false, body["draft"])
			fmt.Fprint(w, `{"id":42,"html_url":"https://gitverse.ru/owner/repo/releases/tag/v1.0.0"}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	ctx := testctx.WrapWithCfg(t.Context(), config.Project{
		GitVerseURLs: config.GitVerseURLs{API: srv.URL},
		Release: config.Release{
			GitVerse: config.Repo{Owner: "owner", Name: "repo"},
		},
	}, testctx.GitVerseTokenType, testctx.WithCurrentTag("v1.0.0"))
	ctx.Config.Release.NameTemplate = "{{.Tag}}"

	client, err := newGitVerse(ctx, ctx.Token)
	require.NoError(t, err)
	releaseID, err := client.CreateRelease(ctx, "body")
	require.NoError(t, err)
	require.Equal(t, "42", releaseID)
}

func TestGitVersePublishReleaseIsNoop(t *testing.T) {
	t.Parallel()
	srv := fakeGitVerse(t, func(_ http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
	})
	ctx := testctx.WrapWithCfg(t.Context(), config.Project{
		GitVerseURLs: config.GitVerseURLs{API: srv.URL},
		Release: config.Release{
			GitVerse: config.Repo{Owner: "owner", Name: "repo"},
		},
	}, testctx.GitVerseTokenType)
	client, err := newGitVerse(ctx, ctx.Token)
	require.NoError(t, err)
	require.NoError(t, client.PublishRelease(ctx, "42"))
}

func TestGitVerseUpload(t *testing.T) {
	t.Parallel()
	var gotMultipart bool
	srv := fakeGitVerse(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/repos/owner/repo/releases/42/assets", r.URL.Path)
		require.Equal(t, "artifact.txt", r.URL.Query().Get("name"))

		require.NoError(t, r.ParseMultipartForm(1024))
		file, header, err := r.FormFile("attachment")
		require.NoError(t, err)
		defer file.Close()
		require.Equal(t, "artifact.txt", header.Filename)
		gotMultipart = true
		fmt.Fprint(w, `{"id":1,"name":"artifact.txt"}`)
	})
	file, err := os.CreateTemp(t.TempDir(), "artifact.txt")
	require.NoError(t, err)
	_, err = file.WriteString("content")
	require.NoError(t, err)
	require.NoError(t, file.Close())

	ctx := testctx.WrapWithCfg(t.Context(), config.Project{
		GitVerseURLs: config.GitVerseURLs{API: srv.URL},
		Release: config.Release{
			GitVerse: config.Repo{Owner: "owner", Name: "repo"},
		},
	}, testctx.GitVerseTokenType)
	client, err := newGitVerse(ctx, ctx.Token)
	require.NoError(t, err)

	err = client.Upload(ctx, "42", &artifact.Artifact{
		Name: "artifact.txt",
		Path: file.Name(),
	})
	require.NoError(t, err)
	require.True(t, gotMultipart)
}

func fakeGitVerse(tb testing.TB, handler http.HandlerFunc) *httptest.Server {
	tb.Helper()
	if handler == nil {
		handler = func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{}`)
		}
	}
	srv := httptest.NewServer(handler)
	tb.Cleanup(srv.Close)
	return srv
}
