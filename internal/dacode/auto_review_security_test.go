package dacode

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
)

func TestApprovalClassifierPromptIsMinimalRedactedAndBounded(t *testing.T) {
	privateKey := "-----BEGIN PRIVATE KEY-----\nfixture-material\n-----END PRIVATE KEY-----"
	commandSecret := "embedded-command-value"
	request := approvalReviewRequest{
		Transcript: "[user, trusted]\nRun the request with Authorization: Bearer " + commandSecret + " and https://user:" + "pass@example.test/path?token=" + commandSecret + "\n" + privateKey,
		Requests: []dagent.ApprovalRequest{{Call: damessage.ToolCall{
			ID: "call", Name: "execute", Arguments: json.RawMessage(`{"command":"curl -H 'Authorization: Bearer embedded-command-value' 'https://user:` + `pass@example.test/path?token=embedded-command-value'","pem":"-----BEGIN PRIVATE KEY-----\\nfixture-material\\n-----END PRIVATE KEY-----"}`),
		}}},
	}
	prompt := buildApprovalBatchPrompt(request)
	for _, secret := range []string{commandSecret, "user:pass", "fixture-material"} {
		if strings.Contains(prompt, secret) {
			t.Fatalf("classifier prompt leaked %q: %s", secret, prompt)
		}
	}
	if !strings.Contains(prompt, "curl") || !strings.Contains(prompt, "tool_call_id") {
		t.Fatalf("classifier prompt lost bounded action intent: %s", prompt)
	}
	if len([]rune(prompt)) > maxApprovalClassifierPromptRunes {
		t.Fatalf("classifier prompt runes = %d", len([]rune(prompt)))
	}
}

func TestReviewTranscriptExcludesAssistantAndToolPayloads(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, true, "")
	model.items = []transcriptItem{
		{kind: itemUser, text: "authorized user intent"},
		{kind: itemAssistant, text: "assistant-private-output"},
		{kind: itemTool, name: "execute", args: `{"secret":"argument-private"}`, text: "tool-private-output"},
	}
	transcript := model.reviewTranscript()
	if !strings.Contains(transcript, "authorized user intent") || strings.Contains(transcript, "assistant-private-output") || strings.Contains(transcript, "argument-private") || strings.Contains(transcript, "tool-private-output") {
		t.Fatalf("minimal review transcript = %q", transcript)
	}
}

func TestApprovalClassifierRejectsOversizedIdentityAndBoundsMetadata(t *testing.T) {
	oversized := strings.Repeat("x", 129)
	if _, err := approvalBatchID([]dagent.ApprovalRequest{{Call: damessage.ToolCall{ID: oversized, Name: "execute"}}}); err == nil {
		t.Fatal("oversized tool-call ID accepted")
	}
	requests := make([]dagent.ApprovalRequest, 0, maxApprovalBatchRequests)
	for index := range maxApprovalBatchRequests {
		requests = append(requests, dagent.ApprovalRequest{
			Call:        damessage.ToolCall{ID: fmt.Sprintf("call-%03d", index), Name: strings.Repeat("n", 128), Arguments: json.RawMessage(`{"value":"ok"}`)},
			Description: strings.Repeat("description ", 10_000),
		})
	}
	prompt := buildApprovalBatchPrompt(approvalReviewRequest{WorkingDir: strings.Repeat("w", 100_000), Transcript: strings.Repeat("trusted ", 100_000), Requests: requests})
	if len([]rune(prompt)) > maxApprovalClassifierPromptRunes {
		t.Fatalf("bounded prompt runes = %d", len([]rune(prompt)))
	}
	for _, request := range requests {
		if !strings.Contains(prompt, request.Call.ID) {
			t.Fatalf("bounded prompt omitted exact ID %q", request.Call.ID)
		}
	}
}

func TestSanitizeAutoReasonRedactsURLsSecretsAndControls(t *testing.T) {
	secret := "unique-review-secret"
	reason := "allow\x1b[2J\nAPI_TOKEN=inline-secret inspect https://user:" + "password@example.test/path?token=value#fragment then " + secret
	got := sanitizeAutoReason(reason, []string{secret})
	for _, forbidden := range []string{"\x1b", "\n", "inline-secret", "password", "token=value", "fragment", secret} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("sanitized reason contains %q: %q", forbidden, got)
		}
	}
	for _, expected := range []string{"API_TOKEN=[redacted]", "https://%2A%2A%2A@example.test/path?token=%5Bredacted%5D", "[redacted]"} {
		if !strings.Contains(got, expected) {
			t.Fatalf("sanitized reason missing %q: %q", expected, got)
		}
	}
}

func TestKnownApprovalSecretsExtractsOnlyBoundedCredentialShapedValues(t *testing.T) {
	secret := "plain-secret-value"
	arguments := json.RawMessage(`{"path":"not-secret-despite-length","api_token":"` + secret + `","nested":{"authorization":{"value":"nested-bearer-value"}},"short_key":"tiny"}`)
	got := knownApprovalSecrets(arguments)
	if len(got) != 2 || got[0] != "nested-bearer-value" || got[1] != secret {
		t.Fatalf("known secrets = %#v", got)
	}
	sanitized := sanitizeAutoReason("reason repeated "+secret+" and nested-bearer-value", got)
	if strings.Contains(sanitized, secret) || strings.Contains(sanitized, "nested-bearer-value") {
		t.Fatalf("known secret survived = %q", sanitized)
	}
	if got := knownApprovalSecrets(json.RawMessage(`{bad`)); len(got) != 0 {
		t.Fatalf("malformed arguments secrets = %#v", got)
	}
}

func TestKnownApprovalSecretsRedactsOverlappingValuesLongestFirst(t *testing.T) {
	got := knownApprovalSecrets(json.RawMessage(`{"api_key":"abcdefgh","refresh_token":"abcdefghijk"}`))
	if len(got) != 2 || got[0] != "abcdefghijk" || got[1] != "abcdefgh" {
		t.Fatalf("ordered secrets = %#v", got)
	}
	if sanitized := sanitizeAutoReason("abcdefghijk", got); sanitized != "[redacted]" {
		t.Fatalf("overlapping secret sanitization = %q", sanitized)
	}
}

func TestSanitizeAutoReasonHasUsefulBoundedFallback(t *testing.T) {
	if got := sanitizeAutoReason("\x00\n\t", nil); got != "The action was not authorized by the user request." {
		t.Fatalf("empty reason fallback = %q", got)
	}
	got := sanitizeAutoReason(strings.Repeat("界", maxAutoReviewReasonRunes+100), nil)
	if utf8.RuneCountInString(got) != maxAutoReviewReasonRunes {
		t.Fatalf("bounded reason runes = %d", utf8.RuneCountInString(got))
	}
}
