package telegram

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/dago/datalon"
)

const (
	defaultAPIBase          = "https://api.telegram.org"
	defaultPollTimeout      = 30 * time.Second
	defaultPollInterval     = time.Second
	defaultRequestTimeout   = 35 * time.Second
	defaultMaxRetryDelay    = time.Minute
	defaultMaxRequestBytes  = 1 << 20
	defaultMaxResponseBytes = 1 << 20
	defaultMaxWebhookBytes  = 1 << 20
	defaultMaxMessageBytes  = 1 << 20
	defaultMaxOutboundBytes = 1 << 20
	defaultMaxMediaBytes    = 1 << 30
	defaultMaxErrorBytes    = 1024
	defaultMaxUpdates       = 100
	maxTelegramTextRunes    = 4096
	offsetFileLimit         = 4096
)

type Options struct {
	APIBase          string
	Exposure         Exposure
	OffsetFile       string
	PollTimeout      time.Duration
	PollInterval     time.Duration
	RequestTimeout   time.Duration
	MaxRetryDelay    time.Duration
	MaxRequestBytes  int64
	MaxResponseBytes int64
	MaxWebhookBytes  int64
	MaxMessageBytes  int
	MaxOutboundBytes int
	MaxMediaBytes    int64
	MaxErrorBytes    int
	MaxUpdates       int
}

// DefaultOptions returns the bounded, environment-independent long-polling
// defaults. The zero Options value normalizes to the same values.
func DefaultOptions() Options {
	return Options{
		APIBase: defaultAPIBase, PollTimeout: defaultPollTimeout,
		PollInterval: defaultPollInterval, RequestTimeout: defaultRequestTimeout,
		MaxRetryDelay: defaultMaxRetryDelay, MaxRequestBytes: defaultMaxRequestBytes,
		MaxResponseBytes: defaultMaxResponseBytes, MaxWebhookBytes: defaultMaxWebhookBytes,
		MaxMessageBytes: defaultMaxMessageBytes, MaxOutboundBytes: defaultMaxOutboundBytes,
		MaxMediaBytes: defaultMaxMediaBytes, MaxErrorBytes: defaultMaxErrorBytes,
		MaxUpdates: defaultMaxUpdates,
	}
}

type deliveryMode uint8

const (
	longPolling deliveryMode = iota
	webhook
)

// Channel is a Telegram Bot API channel. It uses long polling unless
// constructed by NewWebhook; webhook mode implements http.Handler but owns no
// listener or public network endpoint.
type Channel struct {
	token         string
	client        HTTPClient
	options       Options
	mode          deliveryMode
	webhookSecret string

	mu            sync.Mutex
	handler       datalon.Handler
	root          context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	running       bool
	botID         string
	offset        int64
	lastErr       error
	webhookActive int
	webhooksDone  chan struct{}
}

// New constructs a long-polling Telegram channel. The token and caller-owned
// HTTP client are required positional dependencies. Static invalid inputs panic;
// no request is made until Start.
func New(botToken string, client HTTPClient, options Options) *Channel {
	return newChannel(botToken, client, "", longPolling, options)
}

// NewWebhook constructs a caller-hosted webhook channel. webhookSecret is a
// required positional authentication value. The returned Channel is an
// http.Handler; callers retain ownership of TLS, routing, listener lifecycle,
// source-address policy, and Telegram webhook registration.
func NewWebhook(botToken string, client HTTPClient, webhookSecret string, options Options) *Channel {
	if !validWebhookSecret(webhookSecret) {
		panic("telegram: webhook secret must contain 1-256 letters, numbers, underscores, or hyphens")
	}
	return newChannel(botToken, client, webhookSecret, webhook, options)
}

func newChannel(botToken string, client HTTPClient, webhookSecret string, mode deliveryMode, options Options) *Channel {
	if !validToken(botToken) {
		panic("telegram: bot token is required and must not contain URL separators or controls")
	}
	if nilValue(client) {
		panic("telegram: HTTP client is nil")
	}
	options = normalizeOptions(options)
	return &Channel{
		token: botToken, client: client, options: options,
		mode: mode, webhookSecret: webhookSecret,
	}
}

func (*Channel) ID() string { return "telegram" }

// Options returns a defensive copy of the normalized active options.
func (channel *Channel) Options() Options {
	value := channel.options
	value.Exposure = value.Exposure.clone()
	return value
}

// Start authenticates with getMe, loads a configured polling offset, and starts
// long polling. Webhook channels authenticate and register the handler without
// starting background network work.
func (channel *Channel) Start(ctx context.Context, handler datalon.Handler) error {
	if handler == nil {
		panic("telegram: message handler is nil")
	}
	channel.mu.Lock()
	defer channel.mu.Unlock()
	if channel.running {
		return nil
	}
	if channel.webhookActive != 0 {
		return errors.New("telegram: previous webhook work is still stopping")
	}
	offset := int64(0)
	var err error
	if channel.mode == longPolling && channel.options.OffsetFile != "" {
		offset, err = loadOffset(channel.options.OffsetFile)
		if err != nil {
			return err
		}
	}
	var identity telegramUser
	if err := channel.call(ctx, "getMe", struct{}{}, &identity); err != nil {
		return fmt.Errorf("authenticate telegram bot: %w", err)
	}
	if identity.ID <= 0 {
		return fmt.Errorf("%w: getMe result is missing bot ID", ErrAPIResponse)
	}
	root, cancel := context.WithCancel(context.Background())
	channel.handler = handler
	channel.botID = strconv.FormatInt(identity.ID, 10)
	channel.offset = offset
	channel.root = root
	channel.cancel = cancel
	channel.running = true
	channel.lastErr = nil
	if channel.mode == longPolling {
		channel.done = make(chan struct{})
		go channel.poll(root, channel.done)
	}
	return nil
}

// Stop cancels polling and accepted webhook work, then waits for it to observe
// cancellation. It is idempotent. A client that ignores request contexts can
// delay Stop until ctx.
func (channel *Channel) Stop(ctx context.Context) error {
	channel.mu.Lock()
	if !channel.running {
		webhooksDone := channel.webhooksDone
		active := channel.webhookActive
		channel.mu.Unlock()
		if active != 0 {
			select {
			case <-webhooksDone:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	channel.running = false
	cancel := channel.cancel
	done := channel.done
	webhooksDone := channel.webhooksDone
	active := channel.webhookActive
	channel.root, channel.cancel, channel.done, channel.handler = nil, nil, nil, nil
	channel.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if active == 0 {
		return nil
	}
	select {
	case <-webhooksDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Send delivers plain text in Telegram-sized chunks and returns the final
// provider message ID. Markdown parse modes are deliberately not enabled.
func (channel *Channel) Send(ctx context.Context, conversationID, text string) datalon.SendResult {
	if !validConversationID(conversationID) {
		return sendFailure(errors.New("invalid Telegram conversation ID"), false, channel.options.MaxErrorBytes)
	}
	if len(text) > channel.options.MaxOutboundBytes {
		return sendFailure(ErrPayloadTooBig, false, channel.options.MaxErrorBytes)
	}
	chunks := chunkText(text, maxTelegramTextRunes)
	if len(chunks) == 0 {
		return datalon.SendResult{Success: true}
	}
	var sent telegramMessage
	for _, chunk := range chunks {
		err := channel.call(ctx, "sendMessage", map[string]any{
			"chat_id": conversationID,
			"text":    chunk,
		}, &sent)
		if err != nil {
			return sendFailure(err, retryable(err), channel.options.MaxErrorBytes)
		}
	}
	return datalon.SendResult{Success: true, MessageID: strconv.FormatInt(sent.MessageID, 10)}
}

// LastError returns the latest sanitized background polling or handler error.
func (channel *Channel) LastError() error {
	channel.mu.Lock()
	defer channel.mu.Unlock()
	return channel.lastErr
}

func (channel *Channel) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if channel.mode != webhook {
		http.NotFound(writer, request)
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	provided := request.Header.Get("X-Telegram-Bot-Api-Secret-Token")
	if subtle.ConstantTimeCompare([]byte(provided), []byte(channel.webhookSecret)) != 1 {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	channel.mu.Lock()
	if !channel.running {
		channel.mu.Unlock()
		http.Error(writer, "channel unavailable", http.StatusServiceUnavailable)
		return
	}
	root := channel.root
	if channel.webhookActive == 0 {
		channel.webhooksDone = make(chan struct{})
	}
	channel.webhookActive++
	channel.mu.Unlock()
	defer channel.finishWebhook()
	stopBodyClose := context.AfterFunc(root, func() { _ = request.Body.Close() })
	defer stopBodyClose()
	callCtx, cancel := joinedContext(root, request.Context())
	defer cancel()
	payload, err := readBounded(request.Body, channel.options.MaxWebhookBytes)
	if err != nil {
		http.Error(writer, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	var update telegramUpdate
	if json.Unmarshal(payload, &update) != nil {
		http.Error(writer, "invalid update", http.StatusBadRequest)
		return
	}
	if err := channel.dispatch(callCtx, update); err != nil {
		if errors.Is(err, ErrInvalidUpdate) {
			http.Error(writer, "invalid update", http.StatusBadRequest)
		} else {
			http.Error(writer, "handler failed", http.StatusInternalServerError)
		}
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (channel *Channel) poll(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	for {
		channel.mu.Lock()
		offset := channel.offset
		channel.mu.Unlock()
		var updates []telegramUpdate
		err := channel.call(ctx, "getUpdates", map[string]any{
			"offset":          offset,
			"timeout":         int(channel.options.PollTimeout / time.Second),
			"limit":           channel.options.MaxUpdates,
			"allowed_updates": []string{"message", "channel_post"},
		}, &updates)
		if ctx.Err() != nil {
			return
		}
		delay := channel.options.PollInterval
		if err == nil && len(updates) > channel.options.MaxUpdates {
			err = ErrPayloadTooBig
		}
		if err == nil {
			err = channel.processBatch(ctx, updates)
		}
		if err != nil {
			channel.setLastError(err)
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
				delay = apiErr.RetryAfter
			}
		}
		if !wait(ctx, delay) {
			return
		}
	}
}

func (channel *Channel) processBatch(ctx context.Context, updates []telegramUpdate) error {
	channel.mu.Lock()
	current := channel.offset
	channel.mu.Unlock()
	next := current
	for _, update := range updates {
		if update.UpdateID >= 0 && update.UpdateID < current {
			continue
		}
		if err := channel.dispatch(ctx, update); err != nil {
			if errors.Is(err, ErrInvalidUpdate) {
				channel.setLastError(err)
			} else {
				return err
			}
		}
		if update.UpdateID >= next {
			next = update.UpdateID + 1
		}
	}
	if next <= current {
		return nil
	}
	channel.mu.Lock()
	channel.offset = next
	channel.mu.Unlock()
	if channel.options.OffsetFile != "" {
		if err := saveOffset(channel.options.OffsetFile, next); err != nil {
			return err
		}
	}
	return nil
}

func (channel *Channel) dispatch(ctx context.Context, update telegramUpdate) error {
	message, ok, err := channel.mapUpdate(update)
	if err != nil || !ok {
		return err
	}
	channel.mu.Lock()
	handler := channel.handler
	channel.mu.Unlock()
	if handler == nil {
		return errors.New("telegram channel is not running")
	}
	return invokeHandler(ctx, handler, message)
}

func (channel *Channel) mapUpdate(update telegramUpdate) (datalon.Message, bool, error) {
	if update.UpdateID < 0 || update.UpdateID == int64(^uint64(0)>>1) {
		return datalon.Message{}, false, ErrInvalidUpdate
	}
	raw := update.Message
	expectedType := "private"
	if raw == nil {
		raw = update.ChannelPost
		expectedType = "channel"
	}
	if raw == nil {
		return datalon.Message{}, false, nil
	}
	if raw.Chat.ID == 0 || raw.MessageID <= 0 || raw.Chat.Type != expectedType {
		if raw.Chat.Type == "group" || raw.Chat.Type == "supergroup" {
			return datalon.Message{}, false, nil
		}
		return datalon.Message{}, false, ErrInvalidUpdate
	}
	text := raw.Text
	if text == "" {
		text = raw.Caption
	}
	if len(text) > channel.options.MaxMessageBytes {
		return datalon.Message{}, false, ErrInvalidUpdate
	}
	chatID := strconv.FormatInt(raw.Chat.ID, 10)
	senderID := ""
	if raw.From != nil && raw.From.ID > 0 {
		senderID = strconv.FormatInt(raw.From.ID, 10)
	}
	fromSelf := senderID != "" && senderID == channel.botID
	if !channel.options.Exposure.allows(expectedType, chatID, senderID, fromSelf) {
		return datalon.Message{}, false, nil
	}
	metadata := map[string]any{
		"provider":  "telegram",
		"chat_type": expectedType,
		"from_self": fromSelf,
	}
	if media := raw.media(); media != nil {
		if err := channel.addMediaMetadata(metadata, media); err != nil {
			return datalon.Message{}, false, err
		}
	}
	return datalon.Message{
		ConversationID: chatID, Text: text, SenderID: senderID,
		MessageID: strconv.FormatInt(raw.MessageID, 10), Metadata: metadata,
	}, true, nil
}

func (channel *Channel) addMediaMetadata(metadata map[string]any, media *telegramFile) error {
	if len(media.FileID) == 0 || len(media.FileID) > 1024 ||
		len(media.FileName) > 1024 || len(media.MIMEType) > 256 || media.FileSize < 0 {
		return ErrInvalidUpdate
	}
	metadata["media_type"] = media.MediaType
	metadata["file_id"] = media.FileID
	metadata["has_media"] = true
	if media.FileSize > 0 {
		metadata["file_size"] = media.FileSize
	}
	if media.FileName != "" {
		metadata["file_name"] = media.FileName
	}
	if media.MIMEType != "" {
		metadata["mime_type"] = media.MIMEType
	}
	if media.FileSize > channel.options.MaxMediaBytes {
		metadata["has_media"] = false
		metadata["media_error"] = "Telegram media exceeds the configured limit"
		delete(metadata, "file_id")
	}
	return nil
}

type telegramUpdate struct {
	UpdateID    int64            `json:"update_id"`
	Message     *telegramMessage `json:"message"`
	ChannelPost *telegramMessage `json:"channel_post"`
}

type telegramUser struct {
	ID int64 `json:"id"`
}

type telegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type telegramMessage struct {
	MessageID int64          `json:"message_id"`
	From      *telegramUser  `json:"from"`
	Chat      telegramChat   `json:"chat"`
	Text      string         `json:"text"`
	Caption   string         `json:"caption"`
	Photo     []telegramFile `json:"photo"`
	Voice     *telegramFile  `json:"voice"`
	Audio     *telegramFile  `json:"audio"`
	Video     *telegramFile  `json:"video"`
	VideoNote *telegramFile  `json:"video_note"`
	Document  *telegramFile  `json:"document"`
}

type telegramFile struct {
	FileID    string `json:"file_id"`
	FileSize  int64  `json:"file_size"`
	FileName  string `json:"file_name"`
	MIMEType  string `json:"mime_type"`
	MediaType string `json:"-"`
}

func (message telegramMessage) media() *telegramFile {
	if len(message.Photo) > 0 {
		best := message.Photo[0]
		for _, photo := range message.Photo[1:] {
			if photo.FileSize >= best.FileSize {
				best = photo
			}
		}
		best.MediaType = "image"
		return &best
	}
	ordered := []struct {
		mediaType string
		file      *telegramFile
	}{
		{"voice", message.Voice}, {"audio", message.Audio},
		{"video", message.Video}, {"video_note", message.VideoNote},
		{"document", message.Document},
	}
	for _, item := range ordered {
		mediaType, file := item.mediaType, item.file
		if file != nil {
			value := *file
			value.MediaType = mediaType
			if mediaType == "audio" {
				value.MediaType = "voice"
			}
			if mediaType == "video_note" {
				value.MediaType = "video"
			}
			if mediaType == "document" && strings.HasPrefix(value.MIMEType, "video/") {
				value.MediaType = "video"
			}
			if mediaType == "document" && strings.HasPrefix(value.MIMEType, "audio/") {
				value.MediaType = "voice"
			}
			return &value
		}
	}
	return nil
}

func normalizeOptions(options Options) Options {
	defaults := DefaultOptions()
	if options.APIBase == "" {
		options.APIBase = defaults.APIBase
	}
	if options.PollTimeout == 0 {
		options.PollTimeout = defaults.PollTimeout
	}
	if options.PollInterval == 0 {
		options.PollInterval = defaults.PollInterval
	}
	if options.RequestTimeout == 0 {
		options.RequestTimeout = defaults.RequestTimeout
	}
	if options.MaxRetryDelay == 0 {
		options.MaxRetryDelay = defaults.MaxRetryDelay
	}
	if options.MaxRequestBytes == 0 {
		options.MaxRequestBytes = defaults.MaxRequestBytes
	}
	if options.MaxResponseBytes == 0 {
		options.MaxResponseBytes = defaults.MaxResponseBytes
	}
	if options.MaxWebhookBytes == 0 {
		options.MaxWebhookBytes = defaults.MaxWebhookBytes
	}
	if options.MaxMessageBytes == 0 {
		options.MaxMessageBytes = defaults.MaxMessageBytes
	}
	if options.MaxOutboundBytes == 0 {
		options.MaxOutboundBytes = defaults.MaxOutboundBytes
	}
	if options.MaxMediaBytes == 0 {
		options.MaxMediaBytes = defaults.MaxMediaBytes
	}
	if options.MaxErrorBytes == 0 {
		options.MaxErrorBytes = defaults.MaxErrorBytes
	}
	if options.MaxUpdates == 0 {
		options.MaxUpdates = defaults.MaxUpdates
	}
	options.APIBase = strings.TrimRight(options.APIBase, "/")
	validateOptions(options)
	options.Exposure = options.Exposure.clone()
	return options
}

func validateOptions(options Options) {
	parsed, err := url.Parse(options.APIBase)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		len(options.APIBase) > 2048 {
		panic("telegram: API base must be an HTTP(S) origin or trusted base path")
	}
	if options.PollTimeout < 0 || options.PollTimeout > 50*time.Second ||
		options.PollInterval < 0 || options.PollInterval > time.Minute ||
		options.RequestTimeout < 0 || options.RequestTimeout > 2*time.Minute ||
		options.MaxRetryDelay < 0 || options.MaxRetryDelay > 5*time.Minute {
		panic("telegram: duration option is outside its finite range")
	}
	if options.MaxRequestBytes < 1 || options.MaxRequestBytes > 16<<20 ||
		options.MaxResponseBytes < 1 || options.MaxResponseBytes > 16<<20 ||
		options.MaxWebhookBytes < 1 || options.MaxWebhookBytes > 16<<20 ||
		options.MaxMessageBytes < 1 || options.MaxMessageBytes > 1<<20 ||
		options.MaxOutboundBytes < 1 || options.MaxOutboundBytes > 1<<20 ||
		options.MaxMediaBytes < 1 || options.MaxMediaBytes > 1<<30 ||
		options.MaxErrorBytes < 1 || options.MaxErrorBytes > 4096 ||
		options.MaxUpdates < 1 || options.MaxUpdates > 100 ||
		len(options.OffsetFile) > 4096 || strings.ContainsRune(options.OffsetFile, 0) {
		panic("telegram: size or work option is outside its finite range")
	}
}

func validToken(value string) bool {
	return tokenPattern.MatchString(value)
}

func validWebhookSecret(value string) bool {
	if len(value) == 0 || len(value) > 256 {
		return false
	}
	for _, char := range value {
		if char != '_' && char != '-' && (char < 'A' || char > 'Z') &&
			(char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}

func validConversationID(value string) bool {
	return len(value) <= 128 && chatIDPattern.MatchString(value)
}

func retryable(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Retryable() || errors.Is(err, ErrTransport)
}

func sendFailure(err error, retryable bool, limit int) datalon.SendResult {
	return datalon.SendResult{
		Error: boundedText(err.Error(), limit), Retryable: retryable,
	}
}

func chunkText(text string, limit int) []string {
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}
	chunks := make([]string, 0, (len(runes)+limit-1)/limit)
	for len(runes) > 0 {
		size := min(limit, len(runes))
		chunks = append(chunks, string(runes[:size]))
		runes = runes[size:]
	}
	return chunks
}

func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func invokeHandler(ctx context.Context, handler datalon.Handler, message datalon.Message) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("telegram message handler panicked")
		}
	}()
	return handler(ctx, message)
}

func joinedContext(first, second context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(second)
	stop := context.AfterFunc(first, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

func (channel *Channel) setLastError(err error) {
	channel.mu.Lock()
	channel.lastErr = channel.boundedBackgroundError(err)
	channel.mu.Unlock()
}

func (channel *Channel) finishWebhook() {
	channel.mu.Lock()
	channel.webhookActive--
	if channel.webhookActive == 0 && channel.webhooksDone != nil {
		close(channel.webhooksDone)
	}
	channel.mu.Unlock()
}

func (channel *Channel) boundedBackgroundError(err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) || errors.Is(err, ErrPayloadTooBig) ||
		errors.Is(err, ErrInvalidUpdate) || errors.Is(err, ErrTransport) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.New(boundedText(err.Error(), channel.options.MaxErrorBytes))
}

func nilValue(value any) bool {
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

type offsetRecord struct {
	Offset int64 `json:"offset"`
}

func loadOffset(path string) (int64, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("load Telegram offset: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > offsetFileLimit {
		return 0, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("load Telegram offset: %w", err)
	}
	defer file.Close()
	payload, err := io.ReadAll(io.LimitReader(file, offsetFileLimit+1))
	if err != nil || len(payload) > offsetFileLimit {
		return 0, nil
	}
	var record offsetRecord
	if json.Unmarshal(payload, &record) != nil || record.Offset < 0 {
		return 0, nil
	}
	return record.Offset, nil
}

func saveOffset(path string, offset int64) (err error) {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("save Telegram offset: %w", err)
	}
	file, err := os.CreateTemp(parent, ".telegram-offset-*")
	if err != nil {
		return fmt.Errorf("save Telegram offset: %w", err)
	}
	name := file.Name()
	defer func() {
		_ = os.Remove(name)
	}()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("save Telegram offset: %w", err)
	}
	if err := json.NewEncoder(file).Encode(offsetRecord{Offset: offset}); err != nil {
		_ = file.Close()
		return fmt.Errorf("save Telegram offset: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("save Telegram offset: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("save Telegram offset: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("save Telegram offset: %w", err)
	}
	return nil
}

var _ datalon.Channel = (*Channel)(nil)
var _ http.Handler = (*Channel)(nil)
