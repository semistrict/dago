package contexthub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	ls "github.com/langchain-ai/langsmith-go"
)

var repoHandlePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// LangSmithClient implements Client with the Context Hub APIs exposed by the
// LangSmith Go SDK. New repositories are private agent repositories.
type LangSmithClient struct {
	client *ls.Client
}

// NewLangSmith constructs a Context Hub backend using LangSmith's standard
// environment/profile authentication. Supplying a client permits custom SDK
// options; a nil client uses ls.NewClient().
func NewLangSmith(identifier string, client *ls.Client) (*Backend, error) {
	if client == nil {
		client = ls.NewClient()
	}
	transport, err := NewLangSmithClient(client)
	if err != nil {
		return nil, err
	}
	return New(identifier, transport)
}

func NewLangSmithClient(client *ls.Client) (*LangSmithClient, error) {
	if client == nil {
		return nil, fmt.Errorf("context hub client: LangSmith client is required")
	}
	return &LangSmithClient{client: client}, nil
}

func (client *LangSmithClient) Pull(ctx context.Context, identifier string) (Tree, error) {
	owner, name, version, err := parseIdentifier(identifier)
	if err != nil {
		return Tree{}, err
	}
	params := ls.RepoDirectoryListParams{}
	if version != "" && version != "latest" {
		params.Commit = ls.F(version)
	}
	response, err := client.client.Repos.Directories.List(ctx, owner, name, params)
	if isStatus(err, http.StatusNotFound) {
		return Tree{}, ErrRepositoryNotFound
	}
	if err != nil {
		return Tree{}, err
	}
	tree := Tree{CommitHash: response.CommitHash, Entries: make(map[string]Entry, len(response.Files))}
	for path, raw := range response.Files {
		object, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		kind, _ := object["type"].(string)
		switch EntryKind(kind) {
		case EntryFile:
			content, _ := object["content"].(string)
			tree.Entries[path] = Entry{Kind: EntryFile, Content: content}
		case EntryAgent, EntrySkill:
			handle, _ := object["repo_handle"].(string)
			tree.Entries[path] = Entry{Kind: EntryKind(kind), RepoHandle: handle}
		}
	}
	return tree, nil
}

func (client *LangSmithClient) Push(ctx context.Context, identifier string, changes map[string]*Entry, parentCommit string) (PushResult, error) {
	owner, name, _, err := parseIdentifier(identifier)
	if err != nil {
		return PushResult{}, err
	}
	if err := client.ensureAgentRepo(ctx, owner, name); err != nil {
		return PushResult{}, err
	}
	files := make(map[string]interface{}, len(changes))
	for path, entry := range changes {
		if entry == nil {
			files[path] = nil
			continue
		}
		switch entry.Kind {
		case EntryFile:
			files[path] = map[string]interface{}{"type": string(EntryFile), "content": entry.Content}
		case EntryAgent, EntrySkill:
			files[path] = map[string]interface{}{"type": string(entry.Kind), "repo_handle": entry.RepoHandle}
		default:
			return PushResult{}, fmt.Errorf("context hub push: unsupported entry kind %q", entry.Kind)
		}
	}
	params := ls.RepoDirectoryCommitParams{Files: ls.F(files)}
	if parentCommit != "" {
		params.ParentCommit = ls.F(parentCommit)
	}
	response, err := client.client.Repos.Directories.Commit(ctx, owner, name, params)
	if err != nil {
		return PushResult{}, err
	}
	return PushResult{CommitHash: response.Commit.CommitHash}, nil
}

func (client *LangSmithClient) ensureAgentRepo(ctx context.Context, owner, name string) error {
	_, err := client.client.Repos.Get(ctx, owner, name)
	if err == nil {
		return nil
	}
	if !isStatus(err, http.StatusNotFound) {
		return err
	}
	if !repoHandlePattern.MatchString(name) {
		return fmt.Errorf("context hub: invalid repo handle %q", name)
	}
	_, err = client.client.Repos.New(ctx, ls.RepoNewParams{
		IsPublic:   ls.F(false),
		RepoHandle: ls.F(name),
		RepoType:   ls.F(ls.RepoNewParamsRepoTypeAgent),
	})
	if isStatus(err, http.StatusConflict) {
		return nil
	}
	return err
}

func isStatus(err error, status int) bool {
	var apiError *ls.Error
	return errors.As(err, &apiError) && apiError.StatusCode == status
}

func parseIdentifier(identifier string) (owner, name, version string, err error) {
	identifier = strings.TrimSpace(identifier)
	if index := strings.LastIndex(identifier, ":"); index >= 0 {
		version = identifier[index+1:]
		identifier = identifier[:index]
		if version == "" {
			return "", "", "", fmt.Errorf("context hub: invalid identifier %q", identifier+":")
		}
	}
	parts := strings.Split(identifier, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", fmt.Errorf("context hub: identifier must be owner/name")
	}
	return parts[0], parts[1], version, nil
}

var _ Client = (*LangSmithClient)(nil)
