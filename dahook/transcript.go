package dahook

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

const defaultTranscriptLimit = 16 << 20

var unsafeComponent = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// TranscriptRecord is one versioned, redacted JSONL projection.
type TranscriptRecord struct {
	SchemaVersion int             `json:"schema_version"`
	Sequence      int             `json:"sequence"`
	RecordID      string          `json:"record_id"`
	ThreadID      string          `json:"thread_id"`
	AgentID       string          `json:"agent_id,omitempty"`
	Role          string          `json:"role"`
	Content       json.RawMessage `json:"content"`
}

// TranscriptHandle identifies a stable materialized projection revision.
type TranscriptHandle struct {
	Path     string
	Revision string
	ThreadID string
	AgentID  string
}

// TranscriptStoreOptions bounds one materialized transcript.
type TranscriptStoreOptions struct{ MaxBytes int64 }

// TranscriptStore owns per-thread and per-agent hook projections.
type TranscriptStore struct {
	root     string
	maxBytes int64
	mu       sync.Mutex
}

// NewTranscriptStore constructs a private store. The root is required and
// positional; zero options select a 16 MiB per-transcript bound.
func NewTranscriptStore(root string, options TranscriptStoreOptions) *TranscriptStore {
	if root == "" {
		panic("dahook: transcript root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		panic(err)
	}
	if options.MaxBytes < 0 {
		panic("dahook: transcript limit cannot be negative")
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = defaultTranscriptLimit
	}
	return &TranscriptStore{root: absolute, maxBytes: options.MaxBytes}
}

func canonicalFilesystemPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := absolute
	var missing []string
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return resolved, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("dahook: cannot resolve filesystem root")
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// Materialize atomically writes a versioned JSONL projection. Records are
// bounded and secrets in textual content are redacted before persistence.
func (store *TranscriptStore) Materialize(ctx context.Context, threadID, agentID string, records []TranscriptRecord) (TranscriptHandle, error) {
	if threadID == "" {
		return TranscriptHandle{}, fmt.Errorf("dahook: transcript thread id is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return TranscriptHandle{}, err
	}
	canonicalRoot, err := canonicalFilesystemPath(store.root)
	if err != nil {
		return TranscriptHandle{}, err
	}
	store.root = canonicalRoot
	if err := rejectLinkedAncestors(store.root); err != nil {
		return TranscriptHandle{}, err
	}
	if err := os.MkdirAll(store.root, 0o700); err != nil {
		return TranscriptHandle{}, err
	}
	if err := ensurePrivateDirectories(store.root, store.root); err != nil {
		return TranscriptHandle{}, err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(store.root, 0o700); err != nil {
			return TranscriptHandle{}, err
		}
	}
	path := store.path(threadID, agentID)
	if err := ensurePrivateDirectories(store.root, filepath.Dir(path)); err != nil {
		return TranscriptHandle{}, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".transcript-*")
	if err != nil {
		return TranscriptHandle{}, err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if runtime.GOOS != "windows" {
		if err := temporary.Chmod(0o600); err != nil {
			temporary.Close()
			return TranscriptHandle{}, err
		}
	}
	hash := sha256.New()
	writer := bufio.NewWriter(temporary)
	var written int64
	for index, input := range records {
		if err := ctx.Err(); err != nil {
			temporary.Close()
			return TranscriptHandle{}, err
		}
		record := input
		record.SchemaVersion = 1
		record.Sequence = index
		record.ThreadID = threadID
		record.AgentID = agentID
		if record.RecordID == "" {
			record.RecordID = fmt.Sprintf("%s-%d", record.Role, index)
		}
		record.Content = redactJSON(record.Content)
		line, err := json.Marshal(record)
		if err != nil {
			temporary.Close()
			return TranscriptHandle{}, err
		}
		line = append(line, '\n')
		written += int64(len(line))
		if written > store.maxBytes {
			temporary.Close()
			return TranscriptHandle{}, fmt.Errorf("dahook: transcript exceeds %d bytes", store.maxBytes)
		}
		if _, err := writer.Write(line); err != nil {
			temporary.Close()
			return TranscriptHandle{}, err
		}
		_, _ = hash.Write(line)
	}
	if err := writer.Flush(); err != nil {
		temporary.Close()
		return TranscriptHandle{}, err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return TranscriptHandle{}, err
	}
	if err := temporary.Close(); err != nil {
		return TranscriptHandle{}, err
	}
	if err := ctx.Err(); err != nil {
		return TranscriptHandle{}, err
	}
	if err := ensurePrivateDirectories(store.root, filepath.Dir(path)); err != nil {
		return TranscriptHandle{}, err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return TranscriptHandle{}, fmt.Errorf("dahook: transcript destination is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return TranscriptHandle{}, err
	}
	if err := os.Rename(name, path); err != nil {
		return TranscriptHandle{}, err
	}
	return TranscriptHandle{Path: path, Revision: hex.EncodeToString(hash.Sum(nil)), ThreadID: threadID, AgentID: agentID}, nil
}

func rejectLinkedAncestors(path string) error {
	current := filepath.Clean(path)
	for {
		info, err := os.Lstat(current)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("dahook: transcript root contains a linked component")
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func ensurePrivateDirectories(root, destination string) error {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return fmt.Errorf("dahook: transcript root is not a real directory")
	}
	relative, err := filepath.Rel(root, destination)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("dahook: transcript directory escapes root")
	}
	current := root
	for _, component := range strings.Split(relative, string(os.PathSeparator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			if err := os.Mkdir(current, 0o700); err != nil && !os.IsExist(err) {
				return err
			}
			info, statErr = os.Lstat(current)
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("dahook: transcript path contains a linked or non-directory component")
		}
		if runtime.GOOS != "windows" {
			if err := os.Chmod(current, 0o700); err != nil {
				return err
			}
		}
	}
	return nil
}

func (store *TranscriptStore) path(threadID, agentID string) string {
	thread := safeComponent(threadID)
	if agentID == "" {
		return filepath.Join(store.root, thread+".jsonl")
	}
	return filepath.Join(store.root, thread, "agents", safeComponent(agentID)+".jsonl")
}

func safeComponent(value string) string {
	safe := strings.Trim(unsafeComponent.ReplaceAllString(value, "-"), "-.")
	if safe == "" {
		safe = "item"
	}
	sum := sha256.Sum256([]byte(value))
	return safe + "-" + hex.EncodeToString(sum[:6])
}

var (
	secretAssignment = regexp.MustCompile(`(?i)\b([A-Z][A-Z0-9_]*(?:KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL)[A-Z0-9_]*)\s*=\s*([^\s,;&]+)`)
	bearerToken      = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	prefixedToken    = regexp.MustCompile(`(?i)(sk-|gh[pousr]_|github_pat_|glpat-|xox[baprs]-|hf_|npm_)[A-Za-z0-9._-]{8,}`)
	privateKeyBlock  = regexp.MustCompile(`(?s)-----BEGIN [^-\r\n]*PRIVATE KEY-----.*?-----END [^-\r\n]*PRIVATE KEY-----`)
	webURL           = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)
	credentialHeader = regexp.MustCompile(`(?im)^([ \t]*(?:authorization|proxy-authorization|cookie|set-cookie|[a-z0-9_-]*(?:api[-_]?key|key|token|secret|password|credential|signature)[a-z0-9_-]*)[ \t]*:[ \t]*)[^\r\n]*(?:\r?\n[ \t]+[^\r\n]*)*`)
)

func redactJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`null`)
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		value = string(raw)
	}
	value = redactValue(value)
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`"[REDACTED]"`)
	}
	return encoded
}
func redactValue(value any) any {
	switch typed := value.(type) {
	case string:
		return redactText(typed)
	case []any:
		if len(typed) == 2 {
			if name, ok := typed[0].(string); ok && sensitiveTranscriptName(name) {
				typed[1] = "[REDACTED]"
				return typed
			}
		}
		for index := range typed {
			typed[index] = redactValue(typed[index])
		}
		return typed
	case map[string]any:
		if name := structuredCredentialName(typed); sensitiveTranscriptName(name) {
			for key := range typed {
				if strings.EqualFold(key, "value") {
					typed[key] = "[REDACTED]"
				}
			}
		}
		for key, item := range typed {
			if sensitiveTranscriptName(key) {
				typed[key] = "[REDACTED]"
			} else {
				typed[key] = redactValue(item)
			}
		}
		return typed
	default:
		return value
	}
}

func structuredCredentialName(value map[string]any) string {
	for key, item := range value {
		if !strings.EqualFold(key, "name") && !strings.EqualFold(key, "header") {
			continue
		}
		if name, ok := item.(string); ok {
			return name
		}
	}
	return ""
}

func redactText(value string) string {
	value = privateKeyBlock.ReplaceAllString(value, "[REDACTED PRIVATE KEY]")
	value = credentialHeader.ReplaceAllString(value, "$1[REDACTED]")
	value = webURL.ReplaceAllStringFunc(value, redactCredentialURL)
	value = secretAssignment.ReplaceAllString(value, "$1=[REDACTED]")
	value = bearerToken.ReplaceAllString(value, "Bearer [REDACTED]")
	value = prefixedToken.ReplaceAllString(value, "[REDACTED]")
	return value
}

func redactCredentialURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return value
	}
	changed := false
	if parsed.User != nil {
		parsed.User = url.User("REDACTED")
		changed = true
	}
	query := parsed.Query()
	for key := range query {
		if sensitiveTranscriptName(key) {
			query.Set(key, "[REDACTED]")
			changed = true
		}
	}
	if !changed {
		return value
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func sensitiveTranscriptName(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	switch normalized {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "auth", "code", "signature", "sig", "session":
		return true
	default:
		return secretEnvironmentName(normalized)
	}
}
