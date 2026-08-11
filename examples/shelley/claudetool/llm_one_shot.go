package claudetool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	dmessage "github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/datool"
	"github.com/semistrict/dago/examples/shelley/llm/imageutil"
)

// LLMOneShotTool sends a one-shot prompt to an LLM and returns the result.
type LLMOneShotTool struct {
	LLMProvider     LLMServiceProvider
	ModelID         string // The conversation's current model ID (used as default)
	WorkingDir      *MutableWorkingDir
	AvailableModels []AvailableModel // Models the agent can choose from
}

const (
	llmOneShotName = "llm_one_shot"

	// Results longer than this are written to a file.
	llmOneShotMaxInlineLen = 4000
)

// OneShotImageDir is where llm_one_shot saves copies of image prompt files so
// the UI can display them via /api/read (the originals may live anywhere on
// disk, which /api/read must not serve).
const OneShotImageDir = "/tmp/shelley-oneshot-images"

// llmOneShotDescription builds the tool description, including model info when models are available.
func (t *LLMOneShotTool) llmOneShotDescription() string {
	base := `Send a one-shot prompt to an LLM and get a response.

Unlike subagents, this is a single request/response with no conversation history or tools.
Use this for simple LLM tasks like summarization, extraction, classification, reformatting,
or image analysis with a vision-capable model.

The prompt is read from files (to handle large inputs cleanly). prompt_files
is a list of paths: text files are concatenated in order, and image files
(png, jpeg, gif, webp, heic) are attached as images. Attaching images requires a
vision-capable model.
Short results are returned inline; long results are written to a file.`

	if len(t.AvailableModels) > 0 {
		base += "\n\nAvailable models (use the \"model\" parameter to override the default):"
		for _, m := range t.AvailableModels {
			if m.DisplayName != "" && m.DisplayName != m.ID {
				base += fmt.Sprintf("\n- %s (%s)", m.ID, m.DisplayName)
			} else {
				base += fmt.Sprintf("\n- %s", m.ID)
			}
		}
	}

	return base
}

// stringOrList unmarshals from either a JSON string or an array of strings.
type stringOrList []string

func (s *stringOrList) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var one string
		if err := json.Unmarshal(data, &one); err != nil {
			return err
		}
		*s = stringOrList{one}
		return nil
	}
	return json.Unmarshal(data, (*[]string)(s))
}

type llmOneShotInput struct {
	PromptFiles  stringOrList `json:"prompt_files" description:"Paths to files for the prompt. Image files are attached as images. Relative paths resolve from the working directory." jsonschema:"type=array|string"`
	OutputFile   string       `json:"output_file,omitempty" description:"Path to write the response to. If omitted, short responses are returned inline and long responses are written to a temporary file."`
	Model        string       `json:"model,omitempty" description:"LLM model to use. Defaults to the conversation's current model."`
	SystemPrompt string       `json:"system_prompt,omitempty" description:"Optional system prompt to include."`
}

// NativeTool executes the one-shot request through dago's tool and model contracts.
func (t *LLMOneShotTool) NativeTool() datool.Tool {
	options := []datool.Option{datool.WithPropertyType("prompt_files", []string{"array", "string"})}
	if len(t.AvailableModels) == 0 {
		options = append(options, datool.WithoutProperty("model"))
	} else {
		models := make([]string, 0, len(t.AvailableModels))
		for _, model := range t.AvailableModels {
			models = append(models, model.ID)
		}
		options = append(options, datool.WithPropertyEnum("model", models...))
	}
	return datool.MustNew(llmOneShotName, t.llmOneShotDescription(), func(ctx context.Context, input llmOneShotInput) (datool.Result, error) {
		prepared, err := t.prepare(ctx, input)
		if err != nil {
			return datool.Result{}, err
		}
		message := dmessage.Message{Role: dmessage.RoleHuman}
		if strings.TrimSpace(prepared.prompt) != "" {
			message.Content = append(message.Content, dmessage.ContentBlock{Type: dmessage.BlockText, Text: prepared.prompt})
		}
		for _, image := range prepared.images {
			message.Content = append(message.Content, dmessage.ContentBlock{
				Type: dmessage.BlockImage, Data: image.Data, MIMEType: image.MediaType,
			})
		}
		messages := []dmessage.Message{message}
		if input.SystemPrompt != "" {
			messages = append([]dmessage.Message{dmessage.System(input.SystemPrompt)}, messages...)
		}
		started := time.Now()
		response, err := prepared.chat.Invoke(ctx, damodel.Request{Messages: messages})
		finished := time.Now()
		if err != nil {
			return datool.Result{}, fmt.Errorf("LLM request failed: %w", err)
		}
		var inputTokens, outputTokens uint64
		if response.Message.Usage != nil {
			inputTokens = uint64(response.Message.Usage.InputTokens)
			outputTokens = uint64(response.Message.Usage.OutputTokens)
		}
		execution, err := finishOneShot(input, prepared, response.Message.TextContent(), inputTokens, outputTokens)
		if err != nil {
			return datool.Result{}, err
		}
		var artifact json.RawMessage
		if execution.Display != nil {
			artifact, err = json.Marshal(execution.Display)
			if err != nil {
				return datool.Result{}, fmt.Errorf("encode one-shot display: %w", err)
			}
		}
		return datool.Result{
			Content: []dmessage.ContentBlock{{Type: dmessage.BlockText, Text: execution.Output}}, Artifact: artifact,
			OtherUsage: nativePurposedUsage("llm_one_shot", prepared.chat, response.Message.Usage, started, finished),
		}, nil
	}, options...)
}

// isImageData reports whether data looks like an image file.
func isImageData(data []byte) bool {
	return imageutil.IsHEIC(data) || strings.HasPrefix(http.DetectContentType(data), "image/")
}

// saveOneShotImage writes a prepared image to OneShotImageDir so /api/read can
// serve it to the UI (mirroring how browser screenshots are surfaced). Returns
// the saved path, or "" on failure.
func saveOneShotImage(ctx context.Context, prepared imageutil.Prepared) string {
	if err := os.MkdirAll(OneShotImageDir, 0o755); err != nil {
		slog.WarnContext(ctx, "llm_one_shot: failed to create image dir", "error", err)
		return ""
	}
	ext := strings.TrimPrefix(prepared.MediaType, "image/")
	path := filepath.Join(OneShotImageDir, uuid.New().String()+"."+ext)
	if err := os.WriteFile(path, prepared.Data, 0o644); err != nil {
		slog.WarnContext(ctx, "llm_one_shot: failed to save image copy", "error", err)
		return ""
	}
	return path
}

type oneShotPrepared struct {
	modelID string
	wd      string
	chat    damodel.Chat
	prompt  string
	images  []imageutil.Prepared
	display any
}

type oneShotExecution struct {
	Output  string
	Display any
}

func (t *LLMOneShotTool) prepare(ctx context.Context, req llmOneShotInput) (oneShotPrepared, error) {
	promptFiles := []string(req.PromptFiles)
	if len(promptFiles) == 0 {
		return oneShotPrepared{}, fmt.Errorf("prompt_files is required")
	}

	wd := t.WorkingDir.Get()

	// Determine which model to use: explicit choice > conversation's model
	modelID := t.ModelID
	if req.Model != "" {
		if len(t.AvailableModels) > 0 {
			found := false
			for _, am := range t.AvailableModels {
				if am.ID == req.Model {
					found = true
					break
				}
			}
			if !found {
				var ids []string
				for _, am := range t.AvailableModels {
					ids = append(ids, am.ID)
				}
				return oneShotPrepared{}, fmt.Errorf("unknown model %q; available: %s", req.Model, strings.Join(ids, ", "))
			}
		}
		modelID = req.Model
	}
	if modelID == "" {
		return oneShotPrepared{}, fmt.Errorf("no model specified and no default model configured")
	}

	if t.LLMProvider == nil {
		return oneShotPrepared{}, fmt.Errorf("LLM provider not configured")
	}

	chat, err := t.LLMProvider.GetChat(modelID)
	if err != nil {
		return oneShotPrepared{}, fmt.Errorf("failed to get chat model for model %q: %w", modelID, err)
	}

	// Assemble the prompt: concatenate text files in order, attach images.
	var promptText strings.Builder
	var images []imageutil.Prepared
	var displayImages []map[string]any
	for _, pf := range promptFiles {
		path := pf
		if !filepath.IsAbs(path) {
			path = filepath.Join(wd, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return oneShotPrepared{}, fmt.Errorf("failed to read prompt file: %w", err)
		}
		if isImageData(data) {
			profile := chat.Profile()
			if !profile.SupportsImages {
				return oneShotPrepared{}, fmt.Errorf("prompt file %q is an image, but model %q does not support image attachments", pf, modelID)
			}
			prepared, err := imageutil.Prepare(data, path, profile.MaxImageDimension, profile.MaxImageBytes)
			if err != nil {
				return oneShotPrepared{}, fmt.Errorf("invalid image %q: %w", pf, err)
			}
			images = append(images, prepared)
			// Save a copy so the UI can render the image via /api/read.
			// Failures are non-fatal: the request itself is unaffected.
			if saved := saveOneShotImage(ctx, prepared); saved != "" {
				img := map[string]any{
					"url": "/api/read?path=" + url.QueryEscape(saved),
					// Resolved against the tool's working directory, not as the
					// agent wrote it: a relative path in an image comment would
					// resolve against whatever cwd the reader happens to have.
					"path": path,
					// The saved copy is the (possibly downscaled) LLM-facing one,
					// so its dimensions describe the rendered image.
					"width":  prepared.Width,
					"height": prepared.Height,
				}
				// source_* describe the file at "path", the coordinates the UI
				// reports image comments in. Omitted when unknown.
				if prepared.SourceWidth > 0 && prepared.SourceHeight > 0 {
					img["source_width"] = prepared.SourceWidth
					img["source_height"] = prepared.SourceHeight
				}
				if prepared.SourceOrientation != imageutil.OrientationNormal {
					img["source_orientation"] = int(prepared.SourceOrientation)
				}
				displayImages = append(displayImages, img)
			}
			continue
		}
		if !utf8.Valid(data) {
			return oneShotPrepared{}, fmt.Errorf("prompt file %q is neither UTF-8 text nor a supported image format", pf)
		}
		if promptText.Len() > 0 && len(data) > 0 {
			promptText.WriteString("\n")
		}
		promptText.Write(data)
	}
	prompt := promptText.String()
	if strings.TrimSpace(prompt) == "" && len(images) == 0 {
		return oneShotPrepared{}, fmt.Errorf("prompt is empty")
	}
	var display any
	if len(displayImages) > 0 {
		display = map[string]any{"images": displayImages}
	}
	return oneShotPrepared{modelID: modelID, wd: wd, chat: chat, prompt: prompt, images: images, display: display}, nil
}

func finishOneShot(req llmOneShotInput, prepared oneShotPrepared, resultText string, inputTokens, outputTokens uint64) (oneShotExecution, error) {
	outputPath := req.OutputFile
	if !filepath.IsAbs(outputPath) && outputPath != "" {
		outputPath = filepath.Join(prepared.wd, outputPath)
	}

	// If no explicit output file but result is long, write to temp file
	if outputPath == "" && len(resultText) > llmOneShotMaxInlineLen {
		f, err := os.CreateTemp(prepared.wd, "llm-result-*.txt")
		if err != nil {
			f, err = os.CreateTemp("", "llm-result-*.txt")
			if err != nil {
				return oneShotExecution{}, fmt.Errorf("failed to create temp file: %w", err)
			}
		}
		outputPath = f.Name()
		f.Close()
	}

	if outputPath != "" {
		if err := os.WriteFile(outputPath, []byte(resultText), 0o644); err != nil {
			return oneShotExecution{}, fmt.Errorf("failed to write output file: %w", err)
		}
		usage := fmt.Sprintf(" (model: %s, input_tokens: %d, output_tokens: %d)",
			prepared.modelID, inputTokens, outputTokens)
		return oneShotExecution{
			Output:  fmt.Sprintf("Response written to %s (%d bytes)%s", outputPath, len(resultText), usage),
			Display: prepared.display,
		}, nil
	}

	usage := fmt.Sprintf("\n\n---\nmodel: %s, input_tokens: %d, output_tokens: %d",
		prepared.modelID, inputTokens, outputTokens)
	return oneShotExecution{Output: resultText + usage, Display: prepared.display}, nil
}
