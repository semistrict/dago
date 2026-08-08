package claudetool

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"
	dtool "github.com/semistrict/dago/tool"

	"shelley.exe.dev/llm"
	"shelley.exe.dev/llm/llmhttp"
	"shelley.exe.dev/pathutil"
)

// LLMServiceProvider defines the interface for getting LLM services
type LLMServiceProvider interface {
	GetService(modelID string) (llm.Service, error)
	GetAvailableModels() []string
}

// KeywordTool provides keyword search functionality
type KeywordTool struct {
	llmProvider LLMServiceProvider
	workingDir  *MutableWorkingDir
}

// NewKeywordTool creates a new keyword tool with the given LLM provider
func NewKeywordTool(provider LLMServiceProvider) *KeywordTool {
	return &KeywordTool{llmProvider: provider}
}

// NewKeywordToolWithWorkingDir creates a new keyword tool with the given LLM provider and shared working directory
func NewKeywordToolWithWorkingDir(provider LLMServiceProvider, wd *MutableWorkingDir) *KeywordTool {
	return &KeywordTool{llmProvider: provider, workingDir: wd}
}

// Tool returns the LLM tool definition
func (k *KeywordTool) Tool() *llm.Tool {
	return &llm.Tool{
		Name:        keywordName,
		Description: keywordDescription,
		InputSchema: llm.MustSchema(keywordInputSchema),
		Run:         llm.RunJSON(k.keywordRun),
	}
}

// NativeTool runs keyword search and relevance filtering through Dago's tool
// and model contracts. Tool remains the pinned Shelley facade for its tests.
func (k *KeywordTool) NativeTool() dtool.Tool {
	return dtool.Func{
		Spec: dtool.Definition{
			Name: keywordName, Description: keywordDescription,
			InputSchema: json.RawMessage(keywordInputSchema),
		},
		Run: func(ctx context.Context, raw json.RawMessage, _ dtool.Runtime) (dtool.Result, error) {
			var input keywordInput
			if err := json.Unmarshal(raw, &input); err != nil {
				return dtool.Result{}, fmt.Errorf("%w: %v", dtool.ErrInvalidArguments, err)
			}
			search, err := k.search(ctx, input)
			if err != nil {
				return dtool.Result{}, err
			}
			if search.answer != "" {
				return dtool.TextResult(search.answer), nil
			}
			service, err := k.selectBestLLM(k.llmProvider)
			if err != nil {
				return dtool.Result{}, fmt.Errorf("failed to get LLM service: %w", err)
			}
			native, ok := service.(interface{ DagoChat() dmodel.Chat })
			if !ok || native.DagoChat() == nil {
				return dtool.Result{}, fmt.Errorf("keyword relevance model does not expose a native Dago chat")
			}
			chat := native.DagoChat()
			started := time.Now()
			response, err := chat.Invoke(ctx, dmodel.Request{
				Messages: []dmessage.Message{
					dmessage.System(strings.TrimSpace(keywordSystemPrompt)),
					{Role: dmessage.RoleHuman, Content: []dmessage.ContentBlock{
						{Type: dmessage.BlockText, Text: "<pwd>\n" + search.wd + "\n</pwd>"},
						{Type: dmessage.BlockText, Text: "<ripgrep_results>\n" + search.output + "\n</ripgrep_results>"},
						{Type: dmessage.BlockText, Text: "<query>\n" + input.Query + "\n</query>"},
					}},
				},
			})
			finished := time.Now()
			if err != nil {
				return dtool.Result{}, fmt.Errorf("failed to send relevance filtering message: %w", err)
			}
			filtered, err := nativeKeywordText(response.Message)
			if err != nil {
				return dtool.Result{}, err
			}
			k.logResult(ctx, input.Query, search.output, filtered)
			result := dtool.TextResult(filtered)
			result.OtherUsage = nativePurposedUsage("keyword_search", chat, response.Message.Usage, started, finished)
			return result, nil
		},
	}
}

const (
	keywordName        = "keyword_search"
	keywordDescription = `
keyword_search locates files with a search-and-filter approach.
Use when navigating unfamiliar codebases with only conceptual understanding or vague user questions.

Effective use:
- Provide a detailed query for accurate relevance ranking
- Prefer MANY SPECIFIC terms over FEW GENERAL ones (high precision beats high recall)
- Order search terms by importance (most important first)
- Supports regex search terms for flexible matching

IMPORTANT: Do NOT use this tool if you have precise information like log lines, error messages, stack traces, filenames, or symbols. Use direct approaches (rg, cat, etc.) instead.
`

	// If you modify this, update the termui template for prettier rendering.
	keywordInputSchema = `
{
  "type": "object",
  "required": [
    "query",
    "search_terms"
  ],
  "properties": {
    "query": {
      "type": "string",
      "description": "A detailed statement of what you're trying to find or learn."
    },
    "search_terms": {
      "type": "array",
      "items": {
        "type": "string"
      },
      "description": "List of search terms in descending order of importance."
    }
  }
}
`
)

type keywordInput struct {
	Query       string      `json:"query"`
	SearchTerms stringSlice `json:"search_terms"`
}

// stringSlice is a []string that also accepts a single JSON string, since
// models sometimes pass search_terms as a bare string instead of an array.
type stringSlice []string

func (s *stringSlice) UnmarshalJSON(data []byte) error {
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		*s = arr
		return nil
	}
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	*s = stringSlice{str}
	return nil
}

//go:embed keyword_system_prompt.txt
var keywordSystemPrompt string

// FindRepoRoot attempts to find the git repository root from the current directory
func FindRepoRoot(wd string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = wd
	out, err := cmd.Output()
	// todo: cwd here and throughout
	if err != nil {
		return "", fmt.Errorf("failed to find git repository root: %w", err)
	}
	return pathutil.Logical(strings.TrimSpace(string(out)), wd), nil
}

// keywordRun is the main implementation using the LLM provider
func (k *KeywordTool) keywordRun(ctx context.Context, input keywordInput) llm.ToolOut {
	search, err := k.search(ctx, input)
	if err != nil {
		return llm.ErrorToolOut(err)
	}
	if search.answer != "" {
		return llm.ToolOut{LLMContent: llm.TextContent(search.answer)}
	}

	// Select the best available LLM service
	llmService, err := k.selectBestLLM(k.llmProvider)
	if err != nil {
		return llm.ErrorfToolOut("failed to get LLM service: %w", err)
	}

	// Create the filtering request
	system := []llm.SystemContent{
		{Type: "text", Text: strings.TrimSpace(keywordSystemPrompt)},
	}

	initialMessage := llm.Message{
		Role: llm.MessageRoleUser,
		Content: []llm.Content{
			llm.StringContent("<pwd>\n" + search.wd + "\n</pwd>"),
			llm.StringContent("<ripgrep_results>\n" + search.output + "\n</ripgrep_results>"),
			llm.StringContent("<query>\n" + input.Query + "\n</query>"),
		},
	}

	req := &llm.Request{
		Messages: []llm.Message{initialMessage},
		System:   system,
	}

	resp, err := llmService.Do(llmhttp.WithPurpose(ctx, "keyword_search"), req)
	if err != nil {
		return llm.ErrorfToolOut("failed to send relevance filtering message: %w", err)
	}
	// Find the single text content block. Reasoning models may emit Thinking
	// blocks alongside the answer; those are fine to discard. Anything else
	// (or multiple text blocks) is unexpected.
	var filtered string
	var found bool
	for _, c := range resp.Content {
		switch c.Type {
		case llm.ContentTypeThinking, llm.ContentTypeRedactedThinking:
			continue
		case llm.ContentTypeText:
			if found {
				return llm.ErrorfToolOut("multiple text content blocks in relevance filtering response: %v", resp.Content)
			}
			filtered = c.Text
			found = true
		default:
			return llm.ErrorfToolOut("unexpected content type %v in relevance filtering response: %v", c.Type, resp.Content)
		}
	}
	if !found {
		return llm.ErrorfToolOut("no text content in relevance filtering response: %v", resp.Content)
	}

	k.logResult(ctx, input.Query, search.output, filtered)
	return llm.ToolOut{LLMContent: llm.TextContent(filtered)}
}

type keywordSearch struct {
	wd     string
	output string
	answer string
}

func (k *KeywordTool) search(ctx context.Context, input keywordInput) (keywordSearch, error) {
	wd := k.workingDir.Get()
	root, err := FindRepoRoot(wd)
	if err == nil {
		wd = root
	}
	slog.InfoContext(ctx, "keyword search input", "query", input.Query, "keywords", input.SearchTerms, "wd", wd)

	// first remove stopwords
	var keep []string
	for _, term := range input.SearchTerms {
		out, err := ripgrep(ctx, wd, []string{term})
		if err != nil {
			return keywordSearch{}, err
		}
		if len(out) > 64*1024 {
			slog.InfoContext(ctx, "keyword search result too large", "term", term, "bytes", len(out))
			continue
		}
		keep = append(keep, term)
	}

	if len(keep) == 0 {
		return keywordSearch{answer: "each of those search terms yielded too many results"}, nil
	}

	// peel off keywords until we get a result that fits in the query window
	var out string
	for {
		var err error
		out, err = ripgrep(ctx, wd, keep)
		if err != nil {
			return keywordSearch{}, err
		}
		if len(out) < 128*1024 {
			break
		}
		keep = keep[:len(keep)-1]
	}

	return keywordSearch{wd: wd, output: out}, nil
}

func (k *KeywordTool) logResult(ctx context.Context, query, output, filtered string) {
	slog.InfoContext(
		ctx, "keyword search results processed",
		"bytes", len(output),
		"lines", strings.Count(output, "\n"),
		"files", strings.Count(output, "\n\n"),
		"query", query,
		"filtered", filtered,
	)
}

func nativeKeywordText(response dmessage.Message) (string, error) {
	if len(response.ToolCalls) > 0 {
		return "", fmt.Errorf("unexpected tool calls in relevance filtering response: %v", response.ToolCalls)
	}
	var filtered string
	var found bool
	for _, block := range response.Content {
		switch block.Type {
		case dmessage.BlockReasoning:
			continue
		case dmessage.BlockText:
			if found {
				return "", fmt.Errorf("multiple text content blocks in relevance filtering response: %v", response.Content)
			}
			filtered = block.Text
			found = true
		default:
			return "", fmt.Errorf("unexpected content type %v in relevance filtering response: %v", block.Type, response.Content)
		}
	}
	if !found {
		return "", fmt.Errorf("no text content in relevance filtering response: %v", response.Content)
	}
	return filtered, nil
}

func ripgrep(ctx context.Context, wd string, terms []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	args := []string{"-C", "10", "-i", "--line-number", "--with-filename"}
	for _, term := range terms {
		args = append(args, "-e", term)
	}
	cmd := exec.CommandContext(ctx, "rg", args...)
	cmd.Dir = wd
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("search timed out (directory too large to search)")
		}
		// ripgrep returns exit code 1 when no matches are found, which is not an error for us
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return "no matches found", nil
		}
		// Truncate error output to avoid storing enormous data in the conversation.
		errOut := string(out)
		if len(errOut) > 50*1024 {
			errOut = errOut[:50*1024] + "\n... [truncated]"
		}
		return "", fmt.Errorf("search failed: %v\n%s", err, errOut)
	}
	outStr := string(out)
	return outStr, nil
}

// selectBestLLM selects the best available LLM service for keyword search
func (k *KeywordTool) selectBestLLM(provider LLMServiceProvider) (llm.Service, error) {
	for _, model := range PreferredToolModels {
		svc, err := provider.GetService(model)
		if err == nil {
			return svc, nil
		}
	}

	// If no preferred model is available, try any available model
	available := provider.GetAvailableModels()
	if len(available) > 0 {
		return provider.GetService(available[0])
	}

	return nil, fmt.Errorf("no LLM services available")
}
