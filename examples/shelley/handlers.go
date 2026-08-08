package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/backend"
	"github.com/semistrict/dago/checkpoint"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/providers/openai"
)

//go:embed web/*
var webAssets embed.FS

func (application *application) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", application.handleStatus)
	mux.HandleFunc("PUT /api/settings", application.handleSettings)
	mux.HandleFunc("PUT /api/auth/api-key", application.handleAPIKey)
	mux.HandleFunc("POST /api/auth/oauth/start", application.handleOAuthStart)
	mux.HandleFunc("DELETE /api/auth", application.handleAuthClear)
	mux.HandleFunc("GET /api/conversations", application.handleConversationList)
	mux.HandleFunc("POST /api/conversations", application.handleConversationCreate)
	mux.HandleFunc("GET /api/conversations/{id}", application.handleConversationGet)
	mux.HandleFunc("DELETE /api/conversations/{id}", application.handleConversationDelete)
	mux.HandleFunc("POST /api/conversations/{id}/fork", application.handleConversationFork)
	mux.HandleFunc("POST /api/conversations/{id}/messages", application.handleChat)
	mux.HandleFunc("POST /api/conversations/{id}/resume", application.handleResume)
	mux.HandleFunc("POST /api/conversations/{id}/cancel", application.handleCancel)
	mux.HandleFunc("GET /api/files", application.handleFiles)
	mux.HandleFunc("GET /api/file", application.handleFileRead)
	mux.HandleFunc("PUT /api/file", application.handleFileWrite)
	mux.HandleFunc("GET /api/download", application.handleDownload)
	mux.HandleFunc("POST /api/upload", application.handleUpload)
	mux.HandleFunc("GET /api/git", application.handleGit)
	mux.HandleFunc("POST /api/terminal", application.handleTerminal)

	assets, _ := fs.Sub(webAssets, "web")
	files := http.FileServer(http.FS(assets))
	mux.Handle("GET /", noCache(files))
	return requestLog(mux)
}

func (application *application) handleStatus(writer http.ResponseWriter, _ *http.Request) {
	application.mu.RLock()
	configured := application.settings
	mode := "none"
	if application.oauth != nil {
		mode = "subscription"
	} else if application.apiKey != "" {
		mode = "api_key"
	}
	value := map[string]any{
		"ready": mode != "none", "auth_mode": mode, "oauth_state": application.oauthState,
		"oauth_error": application.oauthError, "settings": configured,
		"workspace": application.local.ID(), "running": len(application.running),
	}
	application.mu.RUnlock()
	writeJSON(writer, http.StatusOK, value)
}

func (application *application) handleSettings(writer http.ResponseWriter, request *http.Request) {
	var value settings
	if err := decodeJSONBody(request, &value); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	value.Model = strings.TrimSpace(value.Model)
	value.SandboxName = strings.TrimSpace(value.SandboxName)
	if value.Model == "" || value.Backend != "local" && value.Backend != "langsmith" {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("model and a valid backend are required"))
		return
	}
	if value.Backend == "langsmith" && value.SandboxName == "" {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("sandbox_name is required for LangSmith"))
		return
	}
	application.mu.Lock()
	application.settings = value
	if application.remoteName != value.SandboxName {
		application.remote, application.remoteName = nil, ""
	}
	application.mu.Unlock()
	writeJSON(writer, http.StatusOK, value)
}

func (application *application) handleAPIKey(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		APIKey string `json:"api_key"`
	}
	if err := decodeJSONBody(request, &input); err != nil || strings.TrimSpace(input.APIKey) == "" {
		if err == nil {
			err = fmt.Errorf("api_key is required")
		}
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	application.mu.Lock()
	application.apiKey = strings.TrimSpace(input.APIKey)
	application.oauth = nil
	application.oauthState, application.oauthError = "", ""
	application.mu.Unlock()
	writeJSON(writer, http.StatusOK, map[string]any{"ready": true, "auth_mode": "api_key"})
}

func (application *application) handleOAuthStart(writer http.ResponseWriter, _ *http.Request) {
	application.mu.Lock()
	if application.oauthState == "pending" {
		application.mu.Unlock()
		writeError(writer, http.StatusConflict, fmt.Errorf("subscription sign-in is already pending"))
		return
	}
	application.oauthState, application.oauthError = "pending", ""
	application.mu.Unlock()

	urls := make(chan string, 1)
	results := make(chan struct {
		session *openai.OAuthSession
		err     error
	}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	go func() {
		defer cancel()
		session, err := openai.Login(ctx, openai.OAuthOptions{
			StorePath: path.Join(application.dataDirectory, "openai-oauth.json"),
			OpenURL: func(value string) error {
				select {
				case urls <- value:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
		})
		results <- struct {
			session *openai.OAuthSession
			err     error
		}{session, err}
	}()
	select {
	case url := <-urls:
		go func() {
			result := <-results
			application.mu.Lock()
			defer application.mu.Unlock()
			if result.err != nil {
				application.oauthState, application.oauthError = "failed", result.err.Error()
				return
			}
			application.oauth, application.apiKey = result.session, ""
			application.oauthState, application.oauthError = "complete", ""
		}()
		writeJSON(writer, http.StatusAccepted, map[string]string{"authorization_url": url})
	case result := <-results:
		application.mu.Lock()
		application.oauthState = "failed"
		if result.err != nil {
			application.oauthError = result.err.Error()
		} else {
			application.oauthError = "authorization stopped before producing a URL"
		}
		message := application.oauthError
		application.mu.Unlock()
		writeError(writer, http.StatusBadGateway, fmt.Errorf("%s", message))
	case <-time.After(10 * time.Second):
		cancel()
		application.mu.Lock()
		application.oauthState = "failed"
		application.oauthError = "authorization URL was not produced"
		application.mu.Unlock()
		writeError(writer, http.StatusGatewayTimeout, fmt.Errorf("authorization URL was not produced"))
	}
}

func (application *application) handleAuthClear(writer http.ResponseWriter, _ *http.Request) {
	application.mu.Lock()
	application.apiKey, application.oauth, application.oauthState, application.oauthError = "", nil, "", ""
	application.mu.Unlock()
	_ = os.Remove(path.Join(application.dataDirectory, "openai-oauth.json"))
	writer.WriteHeader(http.StatusNoContent)
}

func (application *application) handleConversationList(writer http.ResponseWriter, request *http.Request) {
	values, err := application.conversations.list(request.Context(), request.URL.Query().Get("q"))
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, values)
}

func (application *application) handleConversationCreate(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Title string `json:"title"`
	}
	if request.Body != nil && request.ContentLength != 0 {
		if err := decodeJSONBody(request, &input); err != nil {
			writeError(writer, http.StatusBadRequest, err)
			return
		}
	}
	application.mu.RLock()
	configured := application.settings
	application.mu.RUnlock()
	value, err := application.conversations.create(request.Context(), input.Title, configured.Model, configured.Backend)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusCreated, value)
}

func (application *application) handleConversationGet(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	value, err := application.conversations.get(request.Context(), id)
	if err != nil {
		writeError(writer, http.StatusNotFound, fmt.Errorf("conversation not found"))
		return
	}
	messages, err := application.messages(request.Context(), id)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"conversation": value, "messages": messages})
}

func (application *application) handleConversationDelete(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	application.cancelRun(id)
	if err := application.checkpoints.DeleteThread(request.Context(), id); err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	if err := application.conversations.delete(request.Context(), id); err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (application *application) handleConversationFork(writer http.ResponseWriter, request *http.Request) {
	source, err := application.conversations.get(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	target, err := application.conversations.create(request.Context(), source.Title+" — fork", source.Model, source.Backend)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	if err := application.checkpoints.CopyThread(request.Context(), source.ID, target.ID); err != nil {
		_ = application.conversations.delete(request.Context(), target.ID)
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusCreated, target)
}

type chatInput struct {
	Message     string   `json:"message"`
	Attachments []string `json:"attachments,omitempty"`
}

func (application *application) handleChat(writer http.ResponseWriter, request *http.Request) {
	var input chatInput
	if err := decodeJSONBody(request, &input); err != nil || strings.TrimSpace(input.Message) == "" {
		if err == nil {
			err = fmt.Errorf("message is required")
		}
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	id := request.PathValue("id")
	conversation, err := application.conversations.get(request.Context(), id)
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	compiled, configured, err := application.buildAgent(request.Context())
	if err != nil {
		writeError(writer, http.StatusPreconditionFailed, err)
		return
	}
	human := message.Human(strings.TrimSpace(input.Message))
	if len(input.Attachments) > 0 {
		files, _, err := application.currentBackend(request.Context())
		if err != nil {
			writeError(writer, http.StatusBadRequest, err)
			return
		}
		for _, attachment := range input.Attachments {
			downloaded := files.Download(request.Context(), []string{attachment})
			if len(downloaded) != 1 || downloaded[0].Error != "" {
				writeError(writer, http.StatusBadRequest, fmt.Errorf("cannot attach %s", attachment))
				return
			}
			blockType := message.BlockFile
			mimeType := contentTypeFor(attachment)
			if strings.HasPrefix(mimeType, "image/") {
				blockType = message.BlockImage
			} else if strings.HasPrefix(mimeType, "audio/") {
				blockType = message.BlockAudio
			}
			human.Content = append(human.Content, message.ContentBlock{Type: blockType, Data: downloaded[0].Content, MIMEType: mimeType, Name: path.Base(attachment)})
		}
	}
	title := ""
	if strings.HasPrefix(conversation.Title, "Untitled") {
		title = input.Message
	}
	_ = application.conversations.touch(request.Context(), id, title, configured.Model, configured.Backend)
	application.streamRun(writer, request, id, compiled, agent.Input{
		Config: checkpoint.Config{ThreadID: id}, Messages: []message.Message{human},
	})
}

func (application *application) handleResume(writer http.ResponseWriter, request *http.Request) {
	var decisions agent.ApprovalResponse
	if err := decodeJSONBody(request, &decisions); err != nil || len(decisions.Decisions) == 0 {
		if err == nil {
			err = fmt.Errorf("decisions are required")
		}
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	compiled, _, err := application.buildAgent(request.Context())
	if err != nil {
		writeError(writer, http.StatusPreconditionFailed, err)
		return
	}
	id := request.PathValue("id")
	application.streamRun(writer, request, id, compiled, agent.Input{Config: checkpoint.Config{ThreadID: id}, Resume: decisions})
}

func (application *application) streamRun(writer http.ResponseWriter, request *http.Request, threadID string, compiled *dago.DeepAgent, input agent.Input) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeError(writer, http.StatusInternalServerError, fmt.Errorf("streaming is unavailable"))
		return
	}
	ctx, finish, err := application.beginRun(threadID, request.Context())
	if err != nil {
		writeError(writer, http.StatusConflict, err)
		return
	}
	defer finish()
	writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-cache, no-transform")
	writer.Header().Set("X-Accel-Buffering", "no")
	stream := compiled.Stream(ctx, input, 64)
	defer stream.Close()
	for {
		event, err := stream.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			sendSSE(writer, "error", map[string]string{"error": err.Error()})
			flusher.Flush()
			return
		}
		sendSSE(writer, "agent", event)
		flusher.Flush()
	}
	result, err := stream.Result(ctx)
	if err != nil {
		sendSSE(writer, "error", map[string]string{"error": err.Error()})
	} else {
		sendSSE(writer, "done", result)
	}
	flusher.Flush()
}

func (application *application) handleCancel(writer http.ResponseWriter, request *http.Request) {
	if !application.cancelRun(request.PathValue("id")) {
		writeError(writer, http.StatusConflict, fmt.Errorf("conversation is not running"))
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (application *application) handleFiles(writer http.ResponseWriter, request *http.Request) {
	files, _, err := application.currentBackend(request.Context())
	if err != nil {
		writeError(writer, http.StatusPreconditionFailed, err)
		return
	}
	filePath := request.URL.Query().Get("path")
	if filePath == "" {
		filePath = "/"
	}
	query := strings.TrimSpace(request.URL.Query().Get("q"))
	if query != "" {
		result, err := files.Glob(request.Context(), "**/*"+query+"*", filePath)
		if err != nil {
			writeError(writer, http.StatusBadRequest, err)
			return
		}
		writeJSON(writer, http.StatusOK, result)
		return
	}
	result, err := files.List(request.Context(), filePath)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (application *application) handleFileRead(writer http.ResponseWriter, request *http.Request) {
	files, _, err := application.currentBackend(request.Context())
	if err != nil {
		writeError(writer, http.StatusPreconditionFailed, err)
		return
	}
	result, err := files.Read(request.Context(), request.URL.Query().Get("path"), 0, 100_000)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (application *application) handleFileWrite(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decodeJSONBody(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	files, _, err := application.currentBackend(request.Context())
	if err == nil {
		_, err = files.Write(request.Context(), input.Path, input.Content)
	}
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"path": input.Path})
}

func (application *application) handleDownload(writer http.ResponseWriter, request *http.Request) {
	filePath := request.URL.Query().Get("path")
	files, _, err := application.currentBackend(request.Context())
	if err != nil {
		writeError(writer, http.StatusPreconditionFailed, err)
		return
	}
	result := files.Download(request.Context(), []string{filePath})
	if len(result) != 1 || result[0].Error != "" {
		writeError(writer, http.StatusNotFound, fmt.Errorf("file is unavailable"))
		return
	}
	writer.Header().Set("Content-Type", contentTypeFor(filePath))
	writer.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", path.Base(filePath)))
	_, _ = writer.Write(result[0].Content)
}

func (application *application) handleUpload(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 25<<20)
	if err := request.ParseMultipartForm(25 << 20); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	file, header, err := request.FormFile("file")
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, (20<<20)+1))
	if err != nil || len(content) > 20<<20 {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("attachment exceeds 20 MiB"))
		return
	}
	filePath := normalizedUploadPath(header.Filename)
	files, _, err := application.currentBackend(request.Context())
	if err != nil {
		writeError(writer, http.StatusPreconditionFailed, err)
		return
	}
	result := files.Upload(request.Context(), []backend.Upload{{Path: filePath, Content: content}})
	if len(result) != 1 || result[0].Error != "" {
		writeError(writer, http.StatusBadRequest, fmt.Errorf("upload failed: %s", result[0].Error))
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"path": filePath, "name": header.Filename, "mime_type": contentTypeFor(header.Filename), "size": len(content)})
}

func (application *application) handleTerminal(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout_seconds"`
	}
	if err := decodeJSONBody(request, &input); err != nil || strings.TrimSpace(input.Command) == "" {
		if err == nil {
			err = fmt.Errorf("command is required")
		}
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	files, _, err := application.currentBackend(request.Context())
	if err != nil {
		writeError(writer, http.StatusPreconditionFailed, err)
		return
	}
	sandbox, ok := files.(backend.Sandbox)
	if !ok {
		writeError(writer, http.StatusNotImplemented, fmt.Errorf("selected backend does not expose execution"))
		return
	}
	if input.Timeout <= 0 {
		input.Timeout = 60
	}
	result, err := sandbox.Execute(request.Context(), input.Command, time.Duration(input.Timeout)*time.Second)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (application *application) handleGit(writer http.ResponseWriter, request *http.Request) {
	files, _, err := application.currentBackend(request.Context())
	if err != nil {
		writeError(writer, http.StatusPreconditionFailed, err)
		return
	}
	sandbox, ok := files.(backend.Sandbox)
	if !ok {
		writeError(writer, http.StatusNotImplemented, fmt.Errorf("selected backend does not expose execution"))
		return
	}
	status, statusErr := sandbox.Execute(request.Context(), "git status --short --branch", 15*time.Second)
	diff, diffErr := sandbox.Execute(request.Context(), "git diff --no-ext-diff --unified=3", 30*time.Second)
	if statusErr != nil || diffErr != nil {
		writeError(writer, http.StatusBadRequest, errorsJoin(statusErr, diffErr))
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": status, "diff": diff})
}

func sendSSE(writer io.Writer, event string, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		data, _ = json.Marshal(map[string]string{"error": err.Error()})
		event = "error"
	}
	fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event, data)
}

func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(writer, request)
	})
}

func requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		next.ServeHTTP(writer, request)
		if strings.HasPrefix(request.URL.Path, "/api/") {
			fmt.Fprintf(os.Stderr, "%s %s %s\n", request.Method, request.URL.Path, time.Since(started).Round(time.Millisecond))
		}
	})
}

func errorsJoin(values ...error) error {
	var messages []string
	for _, value := range values {
		if value != nil {
			messages = append(messages, value.Error())
		}
	}
	return fmt.Errorf("%s", strings.Join(messages, "; "))
}
