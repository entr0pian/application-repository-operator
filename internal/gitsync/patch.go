// Package gitsync applies structured YAML patches to a file in a remote git
// repo via githubapi.Client, retrying on optimistic-concurrency conflicts.
// It has no knowledge of Kubernetes or the ApplicationRepository CRD —
// callers supply the desired mutation as a plain function over a
// yamlpatch.Document.
package gitsync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/entr0pian/application-repository-operator/internal/githubapi"
	"github.com/entr0pian/application-repository-operator/internal/yamlpatch"
)

const (
	maxAttempts     = 5
	initialBackoff  = 200 * time.Millisecond
	backoffMultiple = 2
)

// ErrConflictExhausted is returned by PatchYAML when every attempt was
// rejected as a conflict (someone else committed to path in between our
// fetch and write, every retry). Wrapped in the returned error so callers
// can detect it with errors.Is.
var ErrConflictExhausted = errors.New("gitsync: exhausted attempts after repeated conflicts")

// Result reports the outcome of a PatchYAML call.
type Result struct {
	// Changed is true if mutate reported a change and it was committed.
	// False means the file already matched the desired state — no commit
	// was made.
	Changed bool

	// CommitSHA is the resulting commit sha, set only when Changed is true.
	CommitSHA string
}

// PatchYAML fetches path fresh, parses it, and calls mutate to apply the
// desired change. If mutate returns false (nothing to change), PatchYAML
// returns without writing. Otherwise it writes the result back with
// optimistic concurrency; on a conflict (someone else committed to path
// since the fetch) it discards everything and retries the whole
// fetch-mutate-write cycle from scratch, up to maxAttempts times, since a
// diff computed against a stale base must never be reapplied blindly.
//
// A missing file (githubapi.ErrNotFound) is treated the same as an empty
// file, so PatchYAML can create path on its first write.
func PatchYAML(ctx context.Context, c githubapi.Client, path, commitMessage string, mutate func(*yamlpatch.Document) bool) (Result, error) {
	backoff := initialBackoff
	var lastErr error

	for attempt := range maxAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return Result{}, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= backoffMultiple
		}

		content, sha, err := c.GetFile(ctx, path)
		if err != nil && !errors.Is(err, githubapi.ErrNotFound) {
			return Result{}, fmt.Errorf("gitsync: patch %s: fetch: %w", path, err)
		}

		doc, err := yamlpatch.Parse(content)
		if err != nil {
			return Result{}, fmt.Errorf("gitsync: patch %s: parse: %w", path, err)
		}

		if !mutate(doc) {
			return Result{Changed: false}, nil
		}

		out, err := doc.Bytes()
		if err != nil {
			return Result{}, fmt.Errorf("gitsync: patch %s: encode: %w", path, err)
		}

		commitSHA, err := c.UpdateFile(ctx, path, out, sha, commitMessage)
		if err == nil {
			return Result{Changed: true, CommitSHA: commitSHA}, nil
		}
		if !errors.Is(err, githubapi.ErrConflict) {
			return Result{}, fmt.Errorf("gitsync: patch %s: update: %w", path, err)
		}
		lastErr = err
	}

	return Result{}, fmt.Errorf("gitsync: patch %s: %w after %d attempts: %w", path, ErrConflictExhausted, maxAttempts, lastErr)
}
