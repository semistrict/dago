// Package whatsapp adapts datalon to the packaged loopback WhatsApp Node bridge.
package whatsapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/semistrict/dago/datalon"
)

var (
	// ErrBridgePayloadTooLarge classifies bounded bridge response rejection.
	ErrBridgePayloadTooLarge = errors.New("whatsapp bridge payload exceeds configured limit")
	// ErrMediaRejected classifies unsafe, missing, unsupported, or oversized media.
	ErrMediaRejected = errors.New("whatsapp media rejected")
)

// Status is the most recently observed pairing/connection state.
type Status struct {
	Provider  string `json:"provider"`
	Connected bool   `json:"connected"`
	Detail    string `json:"detail"`
}

// Media is one outbound local attachment. Type must be image or video.
type Media struct {
	Path    string
	Type    string
	Caption string
}

// Channel attaches a datalon host to one authenticated loopback bridge.
type Channel struct {
	transport Transport
	session   string
	options   Options
	exposure  exposurePolicy

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	handler datalon.Handler
	status  Status
	work    sync.WaitGroup
}

// New constructs an attached bridge channel. transport and sessionDir are
// required positional dependencies; static invalid inputs panic and no I/O is
// performed.
func New(transport Transport, sessionDir string, options Options) *Channel {
	if isNil(transport) {
		panic("whatsapp channel: transport is required")
	}
	prepared, exposure := prepareOptions(sessionDir, options)
	return &Channel{
		transport: transport,
		session:   filepath.Clean(sessionDir),
		options:   prepared,
		exposure:  exposure,
		status:    Status{Provider: "whatsapp", Detail: "disconnected"},
	}
}

// ID implements datalon.Channel.
func (channel *Channel) ID() string { return channel.options.ID }

// Start installs the inbound handler and starts bounded poll and health loops.
func (channel *Channel) Start(ctx context.Context, handler datalon.Handler) error {
	if handler == nil {
		return errors.New("whatsapp channel: inbound handler is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := preparePrivateDirectory(channel.session); err != nil {
		return fmt.Errorf("prepare WhatsApp session: %w", err)
	}
	if err := preparePrivateDirectory(channel.options.InboundMediaDir); err != nil {
		return fmt.Errorf("prepare WhatsApp media: %w", err)
	}
	status, err := channel.health(ctx)
	if err != nil {
		return fmt.Errorf("authenticate WhatsApp bridge: %w", err)
	}
	channel.mu.Lock()
	defer channel.mu.Unlock()
	if channel.running {
		channel.handler = handler
		return nil
	}
	root, cancel := context.WithCancel(context.Background())
	channel.cancel = cancel
	channel.handler = handler
	channel.status = status
	channel.running = true
	channel.work.Add(2)
	go channel.pollLoop(root)
	go channel.healthLoop(root)
	return nil
}

// Stop implements datalon.Channel and waits for background work through ctx.
func (channel *Channel) Stop(ctx context.Context) error {
	channel.mu.Lock()
	if !channel.running {
		channel.mu.Unlock()
		return nil
	}
	channel.running = false
	cancel := channel.cancel
	channel.cancel = nil
	channel.handler = nil
	channel.status = Status{Provider: "whatsapp", Detail: "disconnected"}
	channel.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		channel.work.Wait()
		close(done)
	}()
	select {
	case <-done:
		channel.mu.Lock()
		if !channel.running {
			channel.status = Status{Provider: "whatsapp", Detail: "disconnected"}
		}
		channel.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Status returns an owned snapshot of the last bridge health response. The
// `qr_pending` detail is the pairing state; the Node bridge prints its QR to
// operator-visible stdout.
func (channel *Channel) Status() Status {
	channel.mu.Lock()
	defer channel.mu.Unlock()
	return channel.status
}

// Send implements datalon.Channel. It converts conservative Markdown, prepends
// the bot header to every WhatsApp-sized chunk, and returns the last message ID.
func (channel *Channel) Send(ctx context.Context, conversationID, text string) datalon.SendResult {
	if err := validateOutboundText(conversationID, text, channel.options.MaxTextBytes); err != nil {
		return channel.sendFailure(err)
	}
	chunks := chunkWithHeader(text, channel.options.BotHeader)
	if len(chunks) == 0 || len(chunks) > 1_000 {
		return channel.sendFailure(errors.New("whatsapp channel: invalid outbound chunk count"))
	}
	var messageID string
	for _, chunk := range chunks {
		payload, err := channel.transport.Post(ctx, "/send", map[string]any{"chatId": conversationID, "text": chunk})
		if ctxErr := ctx.Err(); ctxErr != nil {
			return channel.sendFailure(ctxErr)
		}
		if err != nil {
			return channel.sendFailure(err)
		}
		if err := channel.validateBridgePayload(payload); err != nil {
			return channel.sendFailure(err)
		}
		result, err := parseBridgeResult(payload)
		if err != nil {
			return channel.sendFailure(err)
		}
		messageID = result.MessageID
	}
	return datalon.SendResult{Success: true, MessageID: messageID}
}

// SendMedia validates and stages one local image or video beneath the bridge's
// confined media directory before delivery.
func (channel *Channel) SendMedia(ctx context.Context, conversationID string, media Media) datalon.SendResult {
	if strings.TrimSpace(conversationID) == "" || len(conversationID) > 1_024 {
		return channel.sendFailure(errors.New("whatsapp channel: conversation ID is required"))
	}
	staged, err := channel.stageMedia(media)
	if err != nil {
		return channel.sendFailure(err)
	}
	payload := map[string]any{
		"chatId": conversationID, "filePath": staged, "mediaType": media.Type,
		"caption": withBotHeader(media.Caption, channel.options.BotHeader),
	}
	response, err := channel.transport.Post(ctx, "/send-media", payload)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return channel.sendFailure(ctxErr)
	}
	if err != nil {
		return channel.sendFailure(err)
	}
	if err := channel.validateBridgePayload(response); err != nil {
		return channel.sendFailure(err)
	}
	result, err := parseBridgeResult(response)
	if err != nil {
		return channel.sendFailure(err)
	}
	return datalon.SendResult{Success: true, MessageID: result.MessageID}
}

func (channel *Channel) pollLoop(ctx context.Context) {
	defer channel.work.Done()
	for {
		_ = channel.pollOnce(ctx)
		if !wait(ctx, channel.options.PollInterval) {
			return
		}
	}
}

func (channel *Channel) healthLoop(ctx context.Context) {
	defer channel.work.Done()
	for {
		status, err := channel.health(ctx)
		if ctx.Err() != nil {
			return
		}
		channel.mu.Lock()
		if err != nil {
			channel.status = Status{Provider: "whatsapp", Detail: "disconnected"}
		} else {
			channel.status = status
		}
		channel.mu.Unlock()
		if !wait(ctx, channel.options.HealthInterval) {
			return
		}
	}
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (channel *Channel) health(ctx context.Context) (Status, error) {
	payload, err := channel.transport.Get(ctx, "/health")
	if err != nil {
		return Status{}, err
	}
	if err := channel.validateBridgePayload(payload); err != nil {
		return Status{}, err
	}
	var response struct {
		Detail string `json:"status"`
	}
	if err := json.Unmarshal(payload, &response); err != nil || strings.TrimSpace(response.Detail) == "" || len(response.Detail) > 128 {
		return Status{}, errors.New("whatsapp bridge health response is invalid")
	}
	return Status{Provider: "whatsapp", Connected: response.Detail == "connected", Detail: response.Detail}, nil
}

func (channel *Channel) pollOnce(ctx context.Context) error {
	payload, err := channel.transport.Get(ctx, "/messages")
	if err != nil {
		return err
	}
	if err := channel.validateBridgePayload(payload); err != nil {
		return err
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(payload, &messages); err != nil {
		return errors.New("whatsapp bridge messages response must be a list")
	}
	if len(messages) > channel.options.MaxMessagesPerPoll {
		return ErrBridgePayloadTooLarge
	}
	parsed := make([]datalon.Message, 0, len(messages))
	for _, raw := range messages {
		message, allowed, err := channel.parseMessage(raw)
		if err != nil {
			return err
		}
		if allowed {
			parsed = append(parsed, message)
		}
	}
	channel.mu.Lock()
	handler := channel.handler
	channel.mu.Unlock()
	if handler == nil {
		return nil
	}
	for _, message := range parsed {
		if err := handler(ctx, message); err != nil {
			return err
		}
	}
	return nil
}

func (channel *Channel) validateBridgePayload(payload []byte) error {
	if int64(len(payload)) > channel.options.MaxBridgeBytes {
		return ErrBridgePayloadTooLarge
	}
	return nil
}

func (channel *Channel) parseMessage(raw json.RawMessage) (datalon.Message, bool, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil || len(values) > 128 {
		return datalon.Message{}, false, errors.New("whatsapp bridge message must be a bounded object")
	}
	conversationID := firstString(values, "chat_id", "chatId")
	if conversationID == "" || len(conversationID) > 1_024 {
		return datalon.Message{}, false, errors.New("whatsapp bridge message is missing a valid chat ID")
	}
	text := firstString(values, "text", "body")
	if len(text) > channel.options.MaxTextBytes || !utf8.ValidString(text) {
		return datalon.Message{}, false, ErrBridgePayloadTooLarge
	}
	senderID := firstString(values, "user_id", "senderId")
	messageID := firstString(values, "message_id", "messageId")
	if len(senderID) > 1_024 || len(messageID) > 1_024 {
		return datalon.Message{}, false, ErrBridgePayloadTooLarge
	}
	fromSelf := firstBool(values, "from_self", "fromSelf")
	messageType := firstString(values, "message_type", "messageType", "mediaType")
	paths := firstStrings(values, "media_paths", "mediaPaths", "mediaUrls", "media_urls")
	mimes := firstStrings(values, "media_mime_types", "mediaMimeTypes", "mimeTypes")
	if len(paths) > channel.options.MaxMediaPaths || len(mimes) > channel.options.MaxMediaPaths {
		return datalon.Message{}, false, ErrBridgePayloadTooLarge
	}
	mediaType := classifyMedia(firstString(values, "media_type", "mediaType"), messageType, mimes)
	metadata := map[string]any{
		"provider": "whatsapp", "message_type": messageType, "media_type": mediaType,
		"chat_name":    firstString(values, "chat_name", "chatName"),
		"chat_type":    firstString(values, "chat_type", "chatType"),
		"chat_id_from": firstString(values, "chat_id_from", "chatIdFrom"),
		"user_name":    firstString(values, "user_name", "senderName"),
		"from_self":    fromSelf,
	}
	keptPaths, keptMimes, mediaError, err := channel.validateInboundMedia(paths, mimes)
	if err != nil {
		return datalon.Message{}, false, err
	}
	hasMedia := firstBool(values, "has_media", "hasMedia") || len(paths) > 0
	metadata["has_media"] = hasMedia && len(keptPaths) > 0
	if len(keptPaths) > 0 {
		metadata["media_paths"] = keptPaths
		metadata["media_path"] = keptPaths[0]
		metadata["media_mime_types"] = keptMimes
		if mediaType == "voice" {
			metadata["voice_path"] = keptPaths[0]
		}
	}
	if mediaError != "" {
		metadata["media_error"] = mediaError
	}
	message := datalon.Message{
		ConversationID: conversationID, Text: text, SenderID: senderID,
		MessageID: messageID, Metadata: metadata,
	}
	return message, channel.exposure.allows(message), nil
}

func (policy exposurePolicy) allows(message datalon.Message) bool {
	switch policy.mode {
	case ExposureOpen:
		return true
	case ExposureSelf:
		if fromSelf, _ := message.Metadata["from_self"].(bool); fromSelf {
			return true
		}
		_, allowed := policy.operators[message.SenderID]
		return allowed
	case ExposureAllowlist:
		if _, allowed := policy.conversations[message.ConversationID]; allowed {
			return true
		}
		for _, pattern := range policy.mentions {
			if pattern.MatchString(message.Text) {
				return true
			}
		}
	}
	return false
}

func (channel *Channel) validateInboundMedia(paths, mimes []string) ([]string, []string, string, error) {
	root, err := filepath.Abs(channel.options.InboundMediaDir)
	if err != nil {
		return nil, nil, "", err
	}
	kept := make([]string, 0, len(paths))
	keptMimes := make([]string, 0, len(paths))
	dropped := false
	for index, mediaPath := range paths {
		if len(mediaPath) > 4_096 || !utf8.ValidString(mediaPath) {
			dropped = true
			continue
		}
		resolved, contained := containedPath(root, mediaPath, false)
		if !contained {
			dropped = true
			continue
		}
		info, statErr := os.Stat(resolved)
		switch {
		case errors.Is(statErr, os.ErrNotExist):
			// Preserve the pinned bridge's in-progress download behavior.
		case statErr != nil || !info.Mode().IsRegular() || info.Size() > channel.options.MaxMediaBytes:
			dropped = true
			continue
		}
		kept = append(kept, resolved)
		if index < len(mimes) && len(mimes[index]) <= 256 && utf8.ValidString(mimes[index]) {
			keptMimes = append(keptMimes, mimes[index])
		}
	}
	if dropped && len(kept) == 0 && len(paths) > 0 {
		return kept, keptMimes, fmt.Sprintf("all media files exceeded %d bytes or failed confinement", channel.options.MaxMediaBytes), nil
	}
	return kept, keptMimes, "", nil
}

func (channel *Channel) stageMedia(media Media) (string, error) {
	if media.Type != "image" && media.Type != "video" {
		return "", fmt.Errorf("%w: unsupported media type %q", ErrMediaRejected, media.Type)
	}
	if len(media.Caption) > channel.options.MaxTextBytes || !utf8.ValidString(media.Caption) {
		return "", fmt.Errorf("%w: caption is invalid", ErrMediaRejected)
	}
	root, err := filepath.Abs(channel.options.OutboundMediaRoot)
	if err != nil {
		return "", fmt.Errorf("%w: resolve outbound root", ErrMediaRejected)
	}
	source, contained := containedPath(root, media.Path, true)
	if !contained {
		return "", fmt.Errorf("%w: media path escapes outbound root", ErrMediaRejected)
	}
	info, err := os.Stat(source)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: media file is unavailable", ErrMediaRejected)
	}
	if info.Size() > channel.options.MaxMediaBytes {
		return "", fmt.Errorf("%w: media exceeds %d bytes", ErrMediaRejected, channel.options.MaxMediaBytes)
	}
	detected := mime.TypeByExtension(strings.ToLower(filepath.Ext(source)))
	if !strings.HasPrefix(detected, media.Type+"/") {
		return "", fmt.Errorf("%w: file type does not match %s", ErrMediaRejected, media.Type)
	}
	inboundRoot, err := filepath.Abs(channel.options.InboundMediaDir)
	if err != nil {
		return "", err
	}
	if resolved, ok := containedPath(inboundRoot, source, false); ok {
		return resolved, nil
	}
	if err := preparePrivateDirectory(inboundRoot); err != nil {
		return "", err
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("stage WhatsApp media: %w", err)
	}
	destination := filepath.Join(inboundRoot, "outbound_"+hex.EncodeToString(random[:])+filepath.Ext(source))
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	written, copyErr := io.Copy(output, io.LimitReader(input, channel.options.MaxMediaBytes+1))
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil || written > channel.options.MaxMediaBytes {
		_ = os.Remove(destination)
		return "", fmt.Errorf("%w: stage media failed", ErrMediaRejected)
	}
	return destination, nil
}

func containedPath(root, candidate string, relativeToRoot bool) (string, bool) {
	rootAbsolute, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	rootResolved, err := filepath.EvalSymlinks(rootAbsolute)
	if err != nil {
		rootResolved = rootAbsolute
	}
	if relativeToRoot && !filepath.IsAbs(candidate) {
		candidate = filepath.Join(rootResolved, candidate)
	}
	candidateAbsolute, err := filepath.Abs(candidate)
	if err != nil {
		return "", false
	}
	candidateResolved, err := filepath.EvalSymlinks(candidateAbsolute)
	if err != nil {
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(candidateAbsolute))
		if parentErr != nil {
			candidateResolved = candidateAbsolute
		} else {
			candidateResolved = filepath.Join(parent, filepath.Base(candidateAbsolute))
		}
	}
	relative, err := filepath.Rel(rootResolved, candidateResolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == "." {
		return "", false
	}
	return candidateResolved, true
}

func parseBridgeResult(payload json.RawMessage) (struct{ MessageID string }, error) {
	var response struct {
		Success    *bool  `json:"success"`
		Error      string `json:"error"`
		MessageID  string `json:"message_id"`
		MessageID2 string `json:"messageId"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		return struct{ MessageID string }{}, errors.New("whatsapp bridge send response is invalid")
	}
	if response.Success != nil && !*response.Success {
		return struct{ MessageID string }{}, errors.New(boundString(firstNonempty(response.Error, "WhatsApp bridge returned an error"), defaultMaxErrorBytes))
	}
	if response.MessageID == "" {
		response.MessageID = response.MessageID2
	}
	return struct{ MessageID string }{MessageID: response.MessageID}, nil
}

func (channel *Channel) sendFailure(err error) datalon.SendResult {
	return datalon.SendResult{Error: boundString(err.Error(), channel.options.MaxErrorBytes), Retryable: retryable(err)}
}

func retryable(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	value := strings.ToLower(err.Error())
	for _, fragment := range []string{"connection", "timeout", "broken pipe", "network", "temporary", "eof"} {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}

var (
	headingPattern = regexp.MustCompile(`(?m)^#{1,6}\s+`)
	linkPattern    = regexp.MustCompile(`\[([^]]+)]\(([^)]+)\)`)
	boldPattern    = regexp.MustCompile(`\*\*([^*]+)\*\*|__([^_]+)__`)
)

func formatMarkdown(text string) string {
	value := headingPattern.ReplaceAllString(text, "")
	value = linkPattern.ReplaceAllString(value, `$1 ($2)`)
	value = boldPattern.ReplaceAllStringFunc(value, func(match string) string {
		if strings.HasPrefix(match, "**") {
			return "*" + strings.TrimSuffix(strings.TrimPrefix(match, "**"), "**") + "*"
		}
		return "*" + strings.TrimSuffix(strings.TrimPrefix(match, "__"), "__") + "*"
	})
	return value
}

func withBotHeader(text, header string) string {
	formattedHeader := "*" + formatMarkdown(header) + "*"
	if text == "" {
		return formattedHeader
	}
	return formattedHeader + "\n" + formatMarkdown(text)
}

func chunkWithHeader(text, header string) []string {
	formatted := formatMarkdown(text)
	header = "*" + formatMarkdown(header) + "*"
	limit := maxTextRunes - utf8.RuneCountInString(header) - 1
	parts := chunkRunes(formatted, limit)
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		result = append(result, header+"\n"+part)
	}
	return result
}

func chunkRunes(text string, limit int) []string {
	runes := []rune(text)
	result := []string{}
	for len(runes) > limit {
		split := limit
		for index := limit - 1; index > 0; index-- {
			if runes[index] == '\n' || runes[index] == ' ' {
				split = index + 1
				break
			}
		}
		part := strings.TrimSpace(string(runes[:split]))
		if part == "" {
			part = string(runes[:limit])
			split = limit
		}
		result = append(result, part)
		runes = []rune(strings.TrimLeft(string(runes[split:]), " \n\t"))
	}
	if part := string(runes); part != "" {
		result = append(result, part)
	}
	return result
}

func validateOutboundText(conversationID, text string, maxBytes int) error {
	if strings.TrimSpace(conversationID) == "" || len(conversationID) > 1_024 || !utf8.ValidString(conversationID) {
		return errors.New("whatsapp channel: conversation ID is required")
	}
	if text == "" || len(text) > maxBytes || !utf8.ValidString(text) {
		return errors.New("whatsapp channel: outbound text is invalid or too large")
	}
	return nil
}

func firstString(values map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		var value string
		if raw, ok := values[key]; ok && json.Unmarshal(raw, &value) == nil && value != "" {
			return value
		}
	}
	return ""
}

func firstBool(values map[string]json.RawMessage, keys ...string) bool {
	for _, key := range keys {
		var value bool
		if raw, ok := values[key]; ok && json.Unmarshal(raw, &value) == nil && value {
			return true
		}
	}
	return false
}

func firstStrings(values map[string]json.RawMessage, keys ...string) []string {
	for _, key := range keys {
		var value []string
		if raw, ok := values[key]; ok && json.Unmarshal(raw, &value) == nil {
			return value
		}
	}
	return nil
}

func classifyMedia(raw, messageType string, mimes []string) string {
	for _, value := range append([]string{raw, messageType}, mimes...) {
		lowered := strings.ToLower(value)
		switch {
		case strings.Contains(lowered, "audio") || lowered == "voice" || lowered == "ptt":
			return "voice"
		case strings.Contains(lowered, "image") || lowered == "photo" || lowered == "sticker":
			return "image"
		case strings.Contains(lowered, "video"):
			return "video"
		}
	}
	return firstNonempty(raw, messageType)
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func preparePrivateDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	return os.Chmod(directory, 0o700)
}

func boundString(value string, limit int) string {
	value = strings.Map(func(current rune) rune {
		if unicode.IsControl(current) && current != '\n' && current != '\t' {
			return '\uFFFD'
		}
		return current
	}, value)
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "..."
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

var _ datalon.Channel = (*Channel)(nil)
