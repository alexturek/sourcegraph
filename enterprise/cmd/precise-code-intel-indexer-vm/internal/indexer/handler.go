package indexer

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/pkg/errors"
	indexmanager "github.com/sourcegraph/sourcegraph/enterprise/cmd/precise-code-intel-indexer-vm/internal/index_manager"
	queue "github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/queue/client"
	"github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/store"
	"github.com/sourcegraph/sourcegraph/internal/workerutil"
)

type Handler struct {
	queueClient  queue.Client
	indexManager *indexmanager.Manager
	commander    Commander
	options      HandlerOptions
}

var _ workerutil.Handler = &Handler{}

type HandlerOptions struct {
	FrontendURL           string
	FrontendURLFromDocker string
	AuthToken             string
}

// Handle clones the target code into a temporary directory, invokes the target indexer in a fresh
// docker container, and uploads the results to the external frontend API.
func (h *Handler) Handle(ctx context.Context, _ workerutil.Store, record workerutil.Record) error {
	index := record.(store.Index)

	h.indexManager.AddID(index.ID)
	defer h.indexManager.RemoveID(index.ID)

	repoDir, err := h.fetchRepository(ctx, index.RepositoryName, index.Commit)
	if err != nil {
		return err
	}
	defer func() {
		_ = os.RemoveAll(repoDir)
	}()

	indexAndUploadCommand := []string{
		"lsif-go",
		"&&",
		"src", "-endpoint", fmt.Sprintf(h.options.FrontendURLFromDocker), "lsif", "upload", "-repo", index.RepositoryName, "-commit", index.Commit,
	}

	if err := h.commander.Run(
		ctx,
		"docker", "run", "--rm",
		"-v", fmt.Sprintf("%s:/data", repoDir),
		"-w", "/data",
		"sourcegraph/lsif-go:latest",
		"bash", "-c", strings.Join(indexAndUploadCommand, " "),
	); err != nil {
		return errors.Wrap(err, "failed to index repository")
	}

	return nil
}

// makeTempDir is a wrapper around ioutil.TempDir that can be replaced during unit tests.
var makeTempDir = func() (string, error) {
	// TMPDIR is set in the dev Procfile to avoid requiring developers to explicitly
	// allow bind mounts of the host's /tmp. If this directory doesn't exist, ioutil.TempDir
	// below will fail.
	if tmpdir := os.Getenv("TMPDIR"); tmpdir != "" {
		if err := os.MkdirAll(tmpdir, os.ModePerm); err != nil {
			return "", err
		}
	}

	return ioutil.TempDir("", "")
}

// fetchRepository creates a temporary directory and performs a git checkout with the given repository
// and commit. If there is an error, the temporary directory is removed.
func (h *Handler) fetchRepository(ctx context.Context, repositoryName, commit string) (string, error) {
	tempDir, err := makeTempDir()
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(tempDir)
		}
	}()

	cloneURL, err := makeCloneURL(h.options.FrontendURL, h.options.AuthToken, repositoryName)
	if err != nil {
		return "", err
	}

	commands := [][]string{
		{"-C", tempDir, "init"},
		{"-C", tempDir, "-c", "protocol.version=2", "fetch", cloneURL.String(), commit},
		{"-C", tempDir, "checkout", commit},
	}

	for _, args := range commands {
		if err := h.commander.Run(ctx, "git", args...); err != nil {
			return "", errors.Wrap(err, fmt.Sprintf("failed `git %s`", strings.Join(args, " ")))
		}
	}

	return tempDir, nil
}

func makeCloneURL(baseURL, authToken, repositoryName string) (*url.URL, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	base.User = url.UserPassword("indexer", authToken)

	return base.ResolveReference(&url.URL{Path: path.Join(".internal-code-intel", "git", repositoryName)}), nil
}
