package dacode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/semistrict/dago/dacredential"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/datool"
	"github.com/semistrict/dago/daweb"
)

func defaultWebTools(ctx context.Context) ([]datool.Tool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory for web tools: %w", err)
	}
	return buildWebTools(
		ctx,
		dacredential.DefaultPath(home),
		os.LookupEnv,
		daweb.NewClient(daweb.Options{}),
	)
}

func buildWebTools(
	ctx context.Context,
	authPath string,
	lookup dacredential.EnvironmentLookup,
	client *daweb.Client,
) ([]datool.Tool, error) {
	if ctx == nil {
		panic("dacode: web tool context is required")
	}
	if lookup == nil {
		panic("dacode: web tool environment lookup is required")
	}
	if client == nil {
		panic("dacode: web tool client is required")
	}
	store := dacredential.NewStore(authPath, time.Now, dacredential.Options{})
	resolution, err := store.Resolve(ctx, "tavily", lookup)
	if err != nil {
		return nil, fmt.Errorf("resolve Tavily credential: %w", err)
	}
	tools := []datool.Tool{daweb.NewFetchURLTool(client)}
	if !resolution.Configured {
		return tools, nil
	}
	if resolution.Credential.Type != dacredential.APIKeyType || resolution.Credential.APIKey == nil {
		return nil, errors.New("stored Tavily credential must be an API key")
	}
	return append(tools, daweb.NewWebSearchTool(client, resolution.Credential.APIKey.Key)), nil
}

func webSearchApprovalRule() dagent.ApprovalRule {
	return dagent.ApprovalRule{
		Pattern: "web_search", Description: "Allow this Tavily web search? This uses API credits.",
		AllowedDecisions: []dagent.ApprovalDecision{dagent.ApprovalApprove, dagent.ApprovalReject},
	}
}

func defaultToolApprovalRules() []dagent.ApprovalRule {
	return append(mutatingToolApprovalRules(), webSearchApprovalRule())
}
