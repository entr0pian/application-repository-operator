// Package githubapi is a small client for the subset of GitHub's Contents
// API the operator needs: reading a file's content+sha and updating it with
// optimistic concurrency.
package githubapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ErrNotFound is returned by GetFile when the path does not exist on Branch.
var ErrNotFound = errors.New("githubapi: file not found")

// ErrConflict is returned by UpdateFile when sha no longer matches the
// file's current content on the server — someone else committed in between.
var ErrConflict = errors.New("githubapi: conflicting update, sha out of date")

// Client is the subset of GitHub's Contents API the operator depends on,
// scoped to a single repo and branch. It is an interface so callers can
// substitute a fake in tests instead of hitting the real GitHub API.
type Client interface {
	// GetFile returns the current content and blob sha of path on Branch.
	GetFile(ctx context.Context, path string) (content []byte, sha string, err error)

	// UpdateFile writes content to path, using sha for optimistic
	// concurrency (the sha last returned by GetFile for the same path).
	// Returns the resulting commit sha on success.
	UpdateFile(ctx context.Context, path string, content []byte, sha, message string) (commitSHA string, err error)
}

// RESTClient is a Client backed by the real GitHub REST API.
type RESTClient struct {
	Owner  string
	Repo   string
	Branch string
	Token  string

	// BaseURL overrides the GitHub API base URL; defaults to
	// https://api.github.com. Intended for tests.
	BaseURL string

	// HTTPClient overrides the HTTP client used for requests; defaults to
	// http.DefaultClient.
	HTTPClient *http.Client
}

var _ Client = (*RESTClient)(nil)

func (c *RESTClient) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return "https://api.github.com"
}

func (c *RESTClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *RESTClient) contentsURL(path string) string {
	return fmt.Sprintf("%s/repos/%s/%s/contents/%s", c.baseURL(), c.Owner, c.Repo, path)
}

func (c *RESTClient) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

type contentResponse struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
	SHA      string `json:"sha"`
}

// GetFile implements Client.
func (c *RESTClient) GetFile(ctx context.Context, path string) ([]byte, string, error) {
	url := c.contentsURL(path) + "?ref=" + c.Branch
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	c.setHeaders(req)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("githubapi: get %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("githubapi: get %s: read response: %w", path, err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		var cr contentResponse
		if err := json.Unmarshal(body, &cr); err != nil {
			return nil, "", fmt.Errorf("githubapi: get %s: decode response: %w", path, err)
		}
		if cr.Encoding != "base64" {
			return nil, "", fmt.Errorf("githubapi: get %s: unexpected encoding %q", path, cr.Encoding)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(cr.Content, "\n", ""))
		if err != nil {
			return nil, "", fmt.Errorf("githubapi: get %s: decode content: %w", path, err)
		}
		return raw, cr.SHA, nil
	case http.StatusNotFound:
		return nil, "", ErrNotFound
	default:
		return nil, "", fmt.Errorf("githubapi: get %s: unexpected status %d: %s", path, resp.StatusCode, string(body))
	}
}

type updateRequest struct {
	Message string `json:"message"`
	Content string `json:"content"`
	SHA     string `json:"sha"`
	Branch  string `json:"branch"`
}

type updateResponse struct {
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

// UpdateFile implements Client.
func (c *RESTClient) UpdateFile(ctx context.Context, path string, content []byte, sha, message string) (string, error) {
	reqBody := updateRequest{
		Message: message,
		Content: base64.StdEncoding.EncodeToString(content),
		SHA:     sha,
		Branch:  c.Branch,
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("githubapi: update %s: encode request: %w", path, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.contentsURL(path), bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("githubapi: update %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("githubapi: update %s: read response: %w", path, err)
	}

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var ur updateResponse
		if err := json.Unmarshal(body, &ur); err != nil {
			return "", fmt.Errorf("githubapi: update %s: decode response: %w", path, err)
		}
		return ur.Commit.SHA, nil
	case http.StatusConflict:
		// GitHub returns 409 specifically when the provided sha doesn't
		// match the file's current sha — i.e. someone else committed since
		// we last read it.
		return "", ErrConflict
	default:
		return "", fmt.Errorf("githubapi: update %s: unexpected status %d: %s", path, resp.StatusCode, string(body))
	}
}
