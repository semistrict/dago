package claudetool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dmessage "github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/datool"
)

// oneShotMockService returns a canned response.
type oneShotMockService struct {
	response string
	onDo     func(damodel.Request)
	profile  damodel.Profile
}

func (m *oneShotMockService) Invoke(_ context.Context, req damodel.Request) (damodel.Response, error) {
	if m.onDo != nil {
		m.onDo(req)
	}
	message := dmessage.Assistant(m.response)
	message.Usage = &dmessage.Usage{InputTokens: 10, OutputTokens: 5}
	return damodel.Response{Message: message}, nil
}
func (m *oneShotMockService) Stream(context.Context, damodel.Request) (damodel.Stream, error) {
	return damodel.EmptyStream{}, nil
}
func (m *oneShotMockService) Profile() damodel.Profile {
	profile := m.profile
	if profile.ContextWindow == 0 {
		profile.ContextWindow = 100000
	}
	profile.SupportsImages = true
	return profile
}

// oneShotMockProvider implements LLMServiceProvider with configurable services.
type oneShotMockProvider struct {
	services map[string]damodel.Chat
}

func (p *oneShotMockProvider) GetChat(modelID string) (damodel.Chat, error) {
	svc, ok := p.services[modelID]
	if !ok {
		return nil, fmt.Errorf("unknown model: %s", modelID)
	}
	return svc, nil
}

type oneShotResult struct {
	Error      error
	LLMContent []dmessage.ContentBlock
	Display    any
}

func runOneShot(tool *LLMOneShotTool, input []byte) oneShotResult {
	result, err := tool.NativeTool().Execute(context.Background(), input, datool.Runtime{})
	var display any
	if len(result.Artifact) > 0 {
		_ = json.Unmarshal(result.Artifact, &display)
	}
	return oneShotResult{Error: err, LLMContent: result.Content, Display: display}
}

func (p *oneShotMockProvider) GetAvailableModels() []string {
	var models []string
	for id := range p.services {
		models = append(models, id)
	}
	return models
}

func TestLLMOneShotShortResult(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("What is 2+2?"), 0o644)

	provider := &oneShotMockProvider{
		services: map[string]damodel.Chat{
			"test-model": &oneShotMockService{response: "4"},
		},
	}

	tool := &LLMOneShotTool{
		LLMProvider:     provider,
		ModelID:         "test-model",
		WorkingDir:      NewMutableWorkingDir(dir),
		AvailableModels: []AvailableModel{{ID: "test-model"}},
	}

	input, _ := json.Marshal(llmOneShotInput{PromptFiles: []string{"prompt.txt"}})
	result := runOneShot(tool, input)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	text := result.LLMContent[0].Text
	if !strings.HasPrefix(text, "4") {
		t.Errorf("expected result to start with '4', got: %s", text)
	}
	if !strings.Contains(text, "test-model") {
		t.Errorf("expected result to contain model name, got: %s", text)
	}
}

func TestLLMOneShotLongResult(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("Generate a long story"), 0o644)

	longResponse := strings.Repeat("word ", 1000) // ~5000 chars

	provider := &oneShotMockProvider{
		services: map[string]damodel.Chat{
			"test-model": &oneShotMockService{response: longResponse},
		},
	}

	tool := &LLMOneShotTool{
		LLMProvider:     provider,
		ModelID:         "test-model",
		WorkingDir:      NewMutableWorkingDir(dir),
		AvailableModels: []AvailableModel{{ID: "test-model"}},
	}

	input, _ := json.Marshal(llmOneShotInput{PromptFiles: []string{"prompt.txt"}})
	result := runOneShot(tool, input)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	text := result.LLMContent[0].Text
	if !strings.Contains(text, "Response written to") {
		t.Errorf("expected file output message, got: %s", text)
	}

	matches, _ := filepath.Glob(filepath.Join(dir, "llm-result-*.txt"))
	if len(matches) != 1 {
		t.Fatalf("expected 1 result file, found %d", len(matches))
	}
	content, _ := os.ReadFile(matches[0])
	if string(content) != longResponse {
		t.Errorf("file content mismatch")
	}
}

func TestLLMOneShotExplicitOutputFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("Hello"), 0o644)

	provider := &oneShotMockProvider{
		services: map[string]damodel.Chat{
			"test-model": &oneShotMockService{response: "Hi"},
		},
	}

	tool := &LLMOneShotTool{
		LLMProvider:     provider,
		ModelID:         "test-model",
		WorkingDir:      NewMutableWorkingDir(dir),
		AvailableModels: []AvailableModel{{ID: "test-model"}},
	}

	input, _ := json.Marshal(llmOneShotInput{PromptFiles: []string{"prompt.txt"}, OutputFile: "output.txt"})
	result := runOneShot(tool, input)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}

	outputPath := filepath.Join(dir, "output.txt")
	text := result.LLMContent[0].Text
	if !strings.Contains(text, outputPath) {
		t.Errorf("expected output path in response, got: %s", text)
	}

	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}
	if string(content) != "Hi" {
		t.Errorf("expected 'Hi', got: %s", string(content))
	}
}

func TestLLMOneShotAlternateModel(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("Hello"), 0o644)

	provider := &oneShotMockProvider{
		services: map[string]damodel.Chat{
			"default-model": &oneShotMockService{response: "from default"},
			"other-model":   &oneShotMockService{response: "from other"},
		},
	}

	tool := &LLMOneShotTool{
		LLMProvider: provider,
		ModelID:     "default-model",
		WorkingDir:  NewMutableWorkingDir(dir),
		AvailableModels: []AvailableModel{
			{ID: "default-model"},
			{ID: "other-model"},
		},
	}

	input, _ := json.Marshal(llmOneShotInput{PromptFiles: []string{"prompt.txt"}, Model: "other-model"})
	result := runOneShot(tool, input)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	text := result.LLMContent[0].Text
	if !strings.Contains(text, "from other") {
		t.Errorf("expected 'from other', got: %s", text)
	}
	if !strings.Contains(text, "other-model") {
		t.Errorf("expected model name in usage, got: %s", text)
	}
}

func TestLLMOneShotUnknownModel(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("Hello"), 0o644)

	provider := &oneShotMockProvider{
		services: map[string]damodel.Chat{
			"test-model": &oneShotMockService{response: "ok"},
		},
	}

	tool := &LLMOneShotTool{
		LLMProvider:     provider,
		ModelID:         "test-model",
		WorkingDir:      NewMutableWorkingDir(dir),
		AvailableModels: []AvailableModel{{ID: "test-model"}},
	}

	input, _ := json.Marshal(llmOneShotInput{PromptFiles: []string{"prompt.txt"}, Model: "bogus-model"})
	result := runOneShot(tool, input)

	if result.Error == nil {
		t.Fatal("expected error for unknown model")
	}
	if !strings.Contains(result.Error.Error(), "unknown model") {
		t.Errorf("expected unknown model error, got: %v", result.Error)
	}
}

func TestLLMOneShotMissingFile(t *testing.T) {
	dir := t.TempDir()

	provider := &oneShotMockProvider{
		services: map[string]damodel.Chat{
			"test-model": &oneShotMockService{response: "ok"},
		},
	}

	tool := &LLMOneShotTool{
		LLMProvider:     provider,
		ModelID:         "test-model",
		WorkingDir:      NewMutableWorkingDir(dir),
		AvailableModels: []AvailableModel{{ID: "test-model"}},
	}

	input, _ := json.Marshal(llmOneShotInput{PromptFiles: []string{"nonexistent.txt"}})
	result := runOneShot(tool, input)

	if result.Error == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(result.Error.Error(), "failed to read prompt file") {
		t.Errorf("expected read error, got: %v", result.Error)
	}
}

func TestLLMOneShotEmptyPrompt(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("  \n  "), 0o644)

	provider := &oneShotMockProvider{
		services: map[string]damodel.Chat{
			"test-model": &oneShotMockService{response: "ok"},
		},
	}

	tool := &LLMOneShotTool{
		LLMProvider:     provider,
		ModelID:         "test-model",
		WorkingDir:      NewMutableWorkingDir(dir),
		AvailableModels: []AvailableModel{{ID: "test-model"}},
	}

	input, _ := json.Marshal(llmOneShotInput{PromptFiles: []string{"prompt.txt"}})
	result := runOneShot(tool, input)

	if result.Error == nil {
		t.Fatal("expected error for empty prompt")
	}
	if !strings.Contains(result.Error.Error(), "prompt is empty") {
		t.Errorf("expected empty prompt error, got: %v", result.Error)
	}
}

func TestLLMOneShotToolDescription(t *testing.T) {
	tool := &LLMOneShotTool{
		LLMProvider: &oneShotMockProvider{},
		ModelID:     "model-a",
		WorkingDir:  NewMutableWorkingDir("/tmp"),
		AvailableModels: []AvailableModel{
			{ID: "model-a"},
			{ID: "model-b", DisplayName: "Model B (fancy)"},
		},
	}

	llmTool := tool.NativeTool().Definition()
	if !strings.Contains(llmTool.Description, "- model-a") {
		t.Errorf("expected model-a in description, got: %s", llmTool.Description)
	}
	if !strings.Contains(llmTool.Description, "- model-b (Model B (fancy))") {
		t.Errorf("expected model-b with display name in description, got: %s", llmTool.Description)
	}
}

func TestLLMOneShotToolSchemaEnum(t *testing.T) {
	tool := &LLMOneShotTool{
		LLMProvider: &oneShotMockProvider{},
		ModelID:     "model-a",
		WorkingDir:  NewMutableWorkingDir("/tmp"),
		AvailableModels: []AvailableModel{
			{ID: "model-a"},
			{ID: "model-b"},
		},
	}

	llmTool := tool.NativeTool().Definition()
	schema := string(llmTool.InputSchema)
	if !strings.Contains(schema, `"enum"`) {
		t.Errorf("expected enum in schema, got: %s", schema)
	}
	if !strings.Contains(schema, `"model-a"`) || !strings.Contains(schema, `"model-b"`) {
		t.Errorf("expected model IDs in enum, got: %s", schema)
	}
}

func TestLLMOneShotToolSchemaNoEnum(t *testing.T) {
	tool := &LLMOneShotTool{
		LLMProvider: &oneShotMockProvider{},
		ModelID:     "model-a",
		WorkingDir:  NewMutableWorkingDir("/tmp"),
	}

	llmTool := tool.NativeTool().Definition()
	schema := string(llmTool.InputSchema)
	if strings.Contains(schema, `"enum"`) {
		t.Errorf("expected no enum in schema when no available models, got: %s", schema)
	}
	if strings.Contains(schema, `"model"`) {
		t.Errorf("expected no model property when no available models, got: %s", schema)
	}
}

func TestLLMOneShotSystemPrompt(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("Hello"), 0o644)

	var capturedReq *damodel.Request
	svc := &oneShotMockService{
		response: "response",
		onDo: func(req damodel.Request) {
			capturedReq = &req
		},
	}

	provider := &oneShotMockProvider{
		services: map[string]damodel.Chat{"test-model": svc},
	}

	tool := &LLMOneShotTool{
		LLMProvider:     provider,
		ModelID:         "test-model",
		WorkingDir:      NewMutableWorkingDir(dir),
		AvailableModels: []AvailableModel{{ID: "test-model"}},
	}

	input, _ := json.Marshal(llmOneShotInput{PromptFiles: []string{"prompt.txt"}, SystemPrompt: "You are a pirate."})
	result := runOneShot(tool, input)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if capturedReq == nil {
		t.Fatal("request not captured")
	}
	if len(capturedReq.Messages) != 2 || capturedReq.Messages[0].Role != dmessage.RoleSystem || capturedReq.Messages[0].TextContent() != "You are a pirate." {
		t.Errorf("expected system prompt, got: %+v", capturedReq.Messages)
	}
}

type noImageOneShotService struct{ oneShotMockService }

func (m *noImageOneShotService) Profile() damodel.Profile {
	profile := m.oneShotMockService.Profile()
	profile.SupportsImages = false
	return profile
}

func writeOneShotPNG(t *testing.T, path string, width int) {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, width, 2))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLLMOneShotStringPromptFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("What is 2+2?"), 0o644)

	var captured *damodel.Request
	tool := &LLMOneShotTool{
		LLMProvider: &oneShotMockProvider{services: map[string]damodel.Chat{
			"test-model": &oneShotMockService{response: "4", onDo: func(req damodel.Request) { captured = &req }},
		}},
		ModelID:    "test-model",
		WorkingDir: NewMutableWorkingDir(dir),
	}

	// prompt_files as a bare string instead of an array.
	input := []byte(`{"prompt_files": "prompt.txt"}`)
	result := runOneShot(tool, input)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if captured == nil || len(captured.Messages) != 1 || captured.Messages[0].Content[0].Text != "What is 2+2?" {
		t.Fatalf("unexpected request: %+v", captured)
	}
}

func TestLLMOneShotConcatenatesTextFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("first part"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("second part"), 0o644)

	var captured *damodel.Request
	tool := &LLMOneShotTool{
		LLMProvider: &oneShotMockProvider{services: map[string]damodel.Chat{
			"test-model": &oneShotMockService{response: "ok", onDo: func(req damodel.Request) { captured = &req }},
		}},
		ModelID:    "test-model",
		WorkingDir: NewMutableWorkingDir(dir),
	}

	input, _ := json.Marshal(llmOneShotInput{PromptFiles: []string{"a.txt", "b.txt"}})
	result := runOneShot(tool, input)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if captured == nil || len(captured.Messages) != 1 || len(captured.Messages[0].Content) != 1 {
		t.Fatalf("unexpected request: %+v", captured)
	}
	if got, want := captured.Messages[0].Content[0].Text, "first part\nsecond part"; got != want {
		t.Errorf("prompt = %q, want %q", got, want)
	}
}

func TestLLMOneShotImagePromptFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("Compare these images"), 0o644)
	relative := filepath.Join(dir, "first.png")
	absolute := filepath.Join(t.TempDir(), "second.png")
	writeOneShotPNG(t, relative, 3)
	writeOneShotPNG(t, absolute, 4)

	var captured *damodel.Request
	tool := &LLMOneShotTool{
		LLMProvider: &oneShotMockProvider{services: map[string]damodel.Chat{
			"vision": &oneShotMockService{response: "done", onDo: func(req damodel.Request) { captured = &req }},
		}},
		ModelID:    "vision",
		WorkingDir: NewMutableWorkingDir(dir),
	}
	input, _ := json.Marshal(llmOneShotInput{
		PromptFiles: []string{"prompt.txt", "first.png", absolute},
	})
	result := runOneShot(tool, input)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if captured == nil || len(captured.Messages) != 1 || len(captured.Messages[0].Content) != 3 {
		t.Fatalf("unexpected request: %+v", captured)
	}
	contents := captured.Messages[0].Content
	if contents[0].Text != "Compare these images" {
		t.Errorf("prompt text = %q", contents[0].Text)
	}
	for i, width := range []int{3, 4} {
		content := contents[i+1]
		if content.MIMEType != "image/png" || len(content.Data) == 0 {
			t.Errorf("image %d = %+v", i, content)
		}
		_ = width
	}

	// Display should carry viewable copies of the images for the UI.
	display, ok := result.Display.(map[string]any)
	if !ok {
		t.Fatalf("display = %#v, want map", result.Display)
	}
	imgs, ok := display["images"].([]any)
	if !ok || len(imgs) != 2 {
		t.Fatalf("display images = %#v, want 2 entries", display["images"])
	}
	for i, rawImage := range imgs {
		img, ok := rawImage.(map[string]any)
		if !ok {
			t.Fatalf("display image %d = %#v", i, rawImage)
		}
		urlStr, _ := img["url"].(string)
		if !strings.HasPrefix(urlStr, "/api/read?path=") {
			t.Errorf("display image %d url = %q", i, urlStr)
		}
		savedPath, err := url.ParseQuery(strings.TrimPrefix(urlStr, "/api/read?"))
		if err != nil {
			t.Fatalf("display image %d url parse: %v", i, err)
		}
		if _, err := os.Stat(savedPath.Get("path")); err != nil {
			t.Errorf("display image %d not saved: %v", i, err)
		}
	}
}

func TestLLMOneShotImageOnlyPrompt(t *testing.T) {
	dir := t.TempDir()
	writeOneShotPNG(t, filepath.Join(dir, "pic.png"), 5)

	var captured *damodel.Request
	tool := &LLMOneShotTool{
		LLMProvider: &oneShotMockProvider{services: map[string]damodel.Chat{
			"vision": &oneShotMockService{response: "a picture", onDo: func(req damodel.Request) { captured = &req }},
		}},
		ModelID:    "vision",
		WorkingDir: NewMutableWorkingDir(dir),
	}
	input, _ := json.Marshal(llmOneShotInput{PromptFiles: []string{"pic.png"}})
	result := runOneShot(tool, input)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if captured == nil || len(captured.Messages[0].Content) != 1 || captured.Messages[0].Content[0].MIMEType != "image/png" {
		t.Fatalf("unexpected request: %+v", captured)
	}
}

func TestLLMOneShotRejectsImageForNonVisionModel(t *testing.T) {
	dir := t.TempDir()
	writeOneShotPNG(t, filepath.Join(dir, "pic.png"), 2)
	tool := &LLMOneShotTool{
		LLMProvider: &oneShotMockProvider{services: map[string]damodel.Chat{
			"text": &noImageOneShotService{},
		}},
		ModelID:    "text",
		WorkingDir: NewMutableWorkingDir(dir),
	}
	input, _ := json.Marshal(llmOneShotInput{PromptFiles: []string{"pic.png"}})
	result := runOneShot(tool, input)
	if result.Error == nil || !strings.Contains(result.Error.Error(), "does not support image attachments") {
		t.Fatalf("unexpected error: %v", result.Error)
	}
}

func TestLLMOneShotPromptFileErrors(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "binary.bin"), []byte{0x00, 0x01, 0xff, 0xfe, 0x00}, 0o644)
	writeOneShotPNG(t, filepath.Join(dir, "truncated.png"), 10)
	data, _ := os.ReadFile(filepath.Join(dir, "truncated.png"))
	os.WriteFile(filepath.Join(dir, "truncated.png"), data[:len(data)/2], 0o644)

	tests := []struct {
		name string
		file string
		want string
	}{
		{name: "missing", file: "missing.png", want: "failed to read prompt file"},
		{name: "binary non-image", file: "binary.bin", want: "neither UTF-8 text nor a supported image"},
		{name: "corrupt image", file: "truncated.png", want: "corrupt or truncated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := &LLMOneShotTool{
				LLMProvider: &oneShotMockProvider{services: map[string]damodel.Chat{
					"vision": &oneShotMockService{response: "unused"},
				}},
				ModelID:    "vision",
				WorkingDir: NewMutableWorkingDir(dir),
			}
			input, _ := json.Marshal(llmOneShotInput{PromptFiles: []string{tt.file}})
			result := runOneShot(tool, input)
			if result.Error == nil || !strings.Contains(result.Error.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", result.Error, tt.want)
			}
		})
	}
}
