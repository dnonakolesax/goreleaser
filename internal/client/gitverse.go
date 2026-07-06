package client

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/caarlos0/log"
	"github.com/dnonakolesax/goreleaser/v2/internal/artifact"
	"github.com/dnonakolesax/goreleaser/v2/internal/changelog"
	"github.com/dnonakolesax/goreleaser/v2/internal/retryx"
	"github.com/dnonakolesax/goreleaser/v2/internal/tmpl"
	"github.com/dnonakolesax/goreleaser/v2/pkg/config"
	"github.com/dnonakolesax/goreleaser/v2/pkg/context"
	gitverse "gitverse.ru/onreza/gitverse-sdk/packages/sdk-go"
)

const (
	DefaultGitVerseAPIURL      = gitverse.DefaultBaseURL
	DefaultGitVerseDownloadURL = "https://gitverse.ru"
)

var _ Client = &gitVerseClient{}

type gitVerseClient struct {
	client *gitverse.Client
}

type gitVerseLoggingTransport struct {
	base http.RoundTripper
}

func (t gitVerseLoggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	log.WithField("method", req.Method).
		WithField("url", req.URL.String()).
		Debug("GitVerse API request")
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		log.WithField("method", req.Method).
			WithField("url", req.URL.String()).
			WithError(err).
			Debug("GitVerse API request failed")
		return nil, err
	}
	log.WithField("method", req.Method).
		WithField("url", req.URL.String()).
		WithField("status", resp.StatusCode).
		Debug("GitVerse API response")
	if resp.StatusCode >= http.StatusBadRequest && resp.Body != nil {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			log.WithField("method", req.Method).
				WithField("url", req.URL.String()).
				WithField("status", resp.StatusCode).
				WithError(readErr).
				Debug("GitVerse API error response body read failed")
		} else {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			log.WithField("method", req.Method).
				WithField("url", req.URL.String()).
				WithField("status", resp.StatusCode).
				WithField("body", string(body)).
				Debug("GitVerse API error response body")
		}
	}
	return resp, nil
}

func newGitVerse(ctx *context.Context, token string) (*gitVerseClient, error) {
	apiURL, err := tmpl.New(ctx).Apply(ctx.Config.GitVerseURLs.API)
	if err != nil {
		return nil, fmt.Errorf("templating GitVerse API URL: %w", err)
	}
	if apiURL == "" {
		apiURL = DefaultGitVerseAPIURL
	}
	if _, err := url.ParseRequestURI(apiURL); err != nil {
		return nil, fmt.Errorf("invalid GitVerse API URL %q: %w", apiURL, err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	transport.TLSClientConfig = &tls.Config{
		//nolint:gosec
		InsecureSkipVerify: ctx.Config.GitVerseURLs.SkipTLSVerify,
	}
	var rt http.RoundTripper = transport
	rt = gitVerseLoggingTransport{base: rt}

	return &gitVerseClient{
		client: gitverse.NewClient(gitverse.ClientConfig{
			BaseURL: apiURL,
			Token:   token,
			HTTPClient: &http.Client{
				Transport: rt,
				Timeout:   gitverse.DefaultTimeout,
			},
		}),
	}, nil
}

func gitVerseDo[T any](ctx *context.Context, fn func() (T, error)) (T, error) {
	return retryx.DoWithData(ctx, ctx.Config.Retry, func() (T, error) {
		result, err := fn()
		return result, gitVerseError(err)
	}, retryx.IsRetriable)
}

func gitVerseError(err error) error {
	if err == nil {
		return nil
	}
	if rle, ok := errors.AsType[*gitverse.RateLimitError](err); ok {
		retryAfter := time.Duration(rle.RetryAfter) * time.Second
		if retryAfter == 0 && !rle.Reset.IsZero() {
			retryAfter = time.Until(rle.Reset)
		}
		return retryx.HTTPError{
			Err:        err,
			Status:     http.StatusTooManyRequests,
			RetryAfter: retryAfter,
		}
	}
	if apiErr, ok := errors.AsType[*gitverse.APIError](err); ok {
		return retryx.HTTPError{Err: err, Status: apiErr.StatusCode}
	}
	return err
}

func gitVerseRepo(ctx *context.Context) config.Repo {
	return ctx.Config.Release.GitVerse
}

func (c *gitVerseClient) CloseMilestone(_ *context.Context, _ Repo, _ string) error {
	return ErrNotImplemented
}

func (c *gitVerseClient) Changelog(ctx *context.Context, repo Repo, prev, current string) ([]ChangelogItem, error) {
	result, err := gitVerseDo(ctx, func() (gitverse.CompareResponse, error) {
		return c.client.CompareCommits(ctx, repo.Owner, repo.Name, prev+"..."+current, &gitverse.QueryOptions{
			PerPage: 100,
		})
	})
	if err != nil {
		return nil, err
	}

	var log []ChangelogItem
	for _, commit := range result.Commits {
		item := ChangelogItem{
			SHA: commit.Sha,
		}
		if commit.Commit != nil {
			item.Message = strings.Split(commit.Commit.Message, "\n")[0]
			item.Authors = append(item.Authors, changelog.ExtractCoAuthors(commit.Commit.Message)...)
			if commit.Commit.Author.Name != nil || commit.Commit.Author.Email != nil {
				item.Authors = append([]Author{{
					Name:  value(commit.Commit.Author.Name),
					Email: value(commit.Commit.Author.Email),
				}}, item.Authors...)
			}
		}
		if author := commit.Author; author != nil {
			item.Authors = append([]Author{{
				Username: author.Login,
			}}, item.Authors...)
		}
		log = append(log, fillDeprecated(item))
	}
	return log, nil
}

func (c *gitVerseClient) CreateFile(
	ctx *context.Context,
	_ config.CommitAuthor,
	repo Repo,
	content []byte,
	path,
	message string,
) error {
	branch := repo.Branch
	if branch == "" {
		r, err := gitVerseDo(ctx, func() (gitverse.Repository, error) {
			return c.client.Get(ctx, repo.Owner, repo.Name)
		})
		if err != nil {
			log.WithField("fileName", path).
				WithField("projectID", repo.String()).
				WithError(err).
				Warn("error checking for default branch, using server default")
		} else {
			branch = r.DefaultBranch
		}
	}

	var sha *string
	current, err := gitVerseDo(ctx, func() (gitverse.ContentsResponse, error) {
		return c.client.GetContent(ctx, repo.Owner, repo.Name, path, &gitverse.QueryOptions{
			Ref: branch,
		})
	})
	if err != nil {
		if !gitVerseIsNotFound(err) {
			return err
		}
	} else {
		sha = &current.Sha
	}

	encoded := base64.StdEncoding.EncodeToString(content)
	_, err = gitVerseDo(ctx, func() (gitverse.FileCreationResponse, error) {
		return c.client.CreateOrUpdateFile(ctx, repo.Owner, repo.Name, path, gitverse.CreateFileParams{
			Branch:  optional(branch),
			Content: &encoded,
			Message: &message,
			Sha:     sha,
		})
	})
	return err
}

func (c *gitVerseClient) CreateRelease(ctx *context.Context, body string) (string, error) {
	repo := gitVerseRepo(ctx)
	tpl := tmpl.New(ctx)
	title, err := tpl.Apply(ctx.Config.Release.NameTemplate)
	if err != nil {
		return "", err
	}
	body = truncateReleaseBody(body)

	release, err := gitVerseDo(ctx, func() (gitverse.Release, error) {
		return c.client.GetByTag(ctx, repo.Owner, repo.Name, ctx.Git.CurrentTag)
	})
	if err != nil && !gitVerseIsNotFound(err) {
		return "", err
	}

	target := ctx.Git.Commit
	if ctx.Config.Release.TargetCommitish != "" {
		target, err = tpl.Apply(ctx.Config.Release.TargetCommitish)
		if err != nil {
			return "", err
		}
	}

	draft := ctx.Config.Release.Draft
	if gitVerseIsNotFound(err) {
		release, err = gitVerseDo(ctx, func() (gitverse.Release, error) {
			return c.client.CreateReleases(ctx, repo.Owner, repo.Name, gitverse.CreateReleaseParams{
				Name:            &title,
				TagName:         &ctx.Git.CurrentTag,
				Body:            &body,
				Draft:           &draft,
				Prerelease:      &ctx.PreRelease,
				TargetCommitish: optional(target),
			})
		})
		if err != nil {
			return "", err
		}
		log.WithField("url", value(release.HtmlUrl)).Info("GitVerse release created")
	} else {
		releaseID, err := gitVerseReleaseID(release)
		if err != nil {
			return "", err
		}
		desc := getReleaseNotes(value(release.Body), body, ctx.Config.Release.ReleaseNotesMode)
		release, err = gitVerseDo(ctx, func() (gitverse.Release, error) {
			return c.client.UpdateReleases(ctx, repo.Owner, repo.Name, releaseID, gitverse.UpdateReleaseParams{
				Name:            &title,
				Body:            &desc,
				Draft:           release.Draft,
				Prerelease:      &ctx.PreRelease,
				TargetCommitish: optional(target),
			})
		})
		if err != nil {
			return "", err
		}
		log.WithField("url", value(release.HtmlUrl)).Info("GitVerse release updated")
	}

	return gitVerseReleaseID(release)
}

func (c *gitVerseClient) PublishRelease(ctx *context.Context, releaseID string) error {
	// GitVerse can upload assets to the release directly; unlike GitHub we do
	// not need to create a temporary draft and publish it afterwards.
	return nil
}

func (c *gitVerseClient) ReleaseURLTemplate(ctx *context.Context) (string, error) {
	downloadURL, err := tmpl.New(ctx).Apply(ctx.Config.GitVerseURLs.Download)
	if err != nil {
		return "", fmt.Errorf("templating GitVerse download URL: %w", err)
	}
	return fmt.Sprintf(
		"%s/%s/%s/releases/download/{{ urlPathEscape .Tag }}/{{ .ArtifactName }}",
		downloadURL,
		ctx.Config.Release.GitVerse.Owner,
		ctx.Config.Release.GitVerse.Name,
	), nil
}

func (c *gitVerseClient) Upload(ctx *context.Context, releaseID string, artifact *artifact.Artifact) error {
	if strings.EqualFold(filepath.Ext(artifact.Name), ".json") {
		log.WithField("name", artifact.Name).
			Debug("skipping GitVerse JSON artifact upload")
		return nil
	}
	repo := gitVerseRepo(ctx)
	return retryx.Do(ctx, ctx.Config.Retry, func() error {
		file, err := os.Open(artifact.Path)
		if err != nil {
			return retryx.Unrecoverable(err)
		}
		defer file.Close()

		path := fmt.Sprintf(
			"/repos/%s/%s/releases/%s/assets?name=%s",
			repo.Owner,
			repo.Name,
			releaseID,
			url.QueryEscape(artifact.Name),
		)
		resp, err := c.client.UploadFile(ctx, path, "attachment", artifact.Name, file)
		if err != nil {
			return gitVerseError(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < http.StatusBadRequest {
			return nil
		}

		apiErr := readGitVerseAPIError(resp)
		if resp.StatusCode == http.StatusUnprocessableEntity && ctx.Config.Release.ReplaceExistingArtifacts {
			if delErr := c.deleteReleaseArtifact(ctx, repo, releaseID, artifact.Name); delErr != nil {
				return retryx.Unrecoverable(delErr)
			}
			return retryx.Retriable(apiErr)
		}
		return gitVerseError(apiErr)
	}, retryx.IsRetriable)
}

func (c *gitVerseClient) deleteReleaseArtifact(ctx *context.Context, repo config.Repo, releaseID, name string) error {
	assets, err := gitVerseDo(ctx, func() ([]gitverse.Attachment, error) {
		return c.client.ListAssets(ctx, repo.Owner, repo.Name, releaseID, &gitverse.QueryOptions{PerPage: 100})
	})
	if err != nil {
		return err
	}
	for _, asset := range assets {
		if value(asset.Name) != name || asset.Id == nil {
			continue
		}
		assetID := strconv.FormatInt(int64(*asset.Id), 10)
		_, err := gitVerseDo(ctx, func() (struct{}, error) {
			return struct{}{}, c.client.DeleteAssets(ctx, repo.Owner, repo.Name, releaseID, assetID)
		})
		return err
	}
	return nil
}

func gitVerseReleaseID(release gitverse.Release) (string, error) {
	if release.Id == nil {
		return "", errors.New("GitVerse release response did not include an id")
	}
	return strconv.FormatInt(int64(*release.Id), 10), nil
}

func gitVerseIsNotFound(err error) bool {
	var apiErr *gitverse.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

func readGitVerseAPIError(resp *http.Response) error {
	apiErr := &gitverse.APIError{
		StatusCode: resp.StatusCode,
		RequestID:  resp.Header.Get("X-Request-Id"),
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		apiErr.Message = "failed to read error response"
		return apiErr
	}
	var errResp struct {
		Message          string `json:"message"`
		DocumentationURL string `json:"documentation_url"`
	}
	if err := json.Unmarshal(body, &errResp); err == nil {
		apiErr.Message = errResp.Message
		apiErr.DocumentationURL = errResp.DocumentationURL
	} else {
		apiErr.Message = string(body)
	}
	return apiErr
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func value(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
