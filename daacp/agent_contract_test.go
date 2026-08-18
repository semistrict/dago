package daacp

import (
	"encoding/base64"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
)

func TestStopReasonProjectsModelOutcomes(t *testing.T) {
	maxTokens := damessage.Assistant("truncated")
	damodel.SetOutcome(&maxTokens, damodel.FinishReasonMaxTokens, nil)
	refusal := damessage.Assistant("refused")
	damodel.SetOutcome(&refusal, damodel.FinishReasonRefusal, &damodel.Refusal{Category: "policy"})
	tests := []struct {
		name   string
		result dagent.Result
		want   acp.StopReason
	}{
		{name: "no messages", want: acp.StopReasonEndTurn},
		{name: "ordinary assistant", result: dagent.Result{Messages: []damessage.Message{damessage.Assistant("done")}}, want: acp.StopReasonEndTurn},
		{name: "max tokens", result: dagent.Result{Messages: []damessage.Message{maxTokens}}, want: acp.StopReasonMaxTokens},
		{name: "refusal", result: dagent.Result{Messages: []damessage.Message{refusal}}, want: acp.StopReasonRefusal},
		{name: "last assistant wins", result: dagent.Result{Messages: []damessage.Message{refusal, damessage.Human("ignored"), maxTokens}}, want: acp.StopReasonMaxTokens},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := stopReason(test.result); got != test.want {
				t.Fatalf("stopReason = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPromptMessageRejectsMalformedContentBlocks(t *testing.T) {
	agent := newProtocolAgent(t.Context(), nil, nil, Options{ImagePrompts: true, AudioPrompts: true, EmbeddedContext: true})
	emptyResource := acp.ResourceBlock(acp.EmbeddedResourceResource{})
	tests := []struct {
		name  string
		block acp.ContentBlock
	}{
		{name: "invalid image base64", block: acp.ImageBlock("not base64", "image/png")},
		{name: "invalid audio base64", block: acp.AudioBlock("not base64", "audio/wav")},
		{name: "empty embedded resource", block: emptyResource},
		{name: "empty union", block: acp.ContentBlock{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := agent.promptMessage([]acp.ContentBlock{test.block}); err == nil {
				t.Fatalf("block %#v was accepted", test.block)
			}
		})
	}
}

func TestPromptMessageConvertsBlobResourceAndLinkTitle(t *testing.T) {
	agent := newProtocolAgent(t.Context(), nil, nil, Options{EmbeddedContext: true})
	title := "Guide"
	mime := "application/octet-stream"
	link := acp.ResourceLinkBlock("fallback", "file:///guide")
	link.ResourceLink.Title = &title
	message, err := agent.promptMessage([]acp.ContentBlock{
		link,
		acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{
			Uri: "file:///data.bin", Blob: base64.StdEncoding.EncodeToString([]byte("payload")), MimeType: &mime,
		}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(message.Content) != 2 || message.Content[0].Text != "[Guide](file:///guide)" || message.Content[1].Type != damessage.BlockFile || string(message.Content[1].Data) != "payload" || message.Content[1].MIMEType != mime {
		t.Fatalf("message = %#v", message)
	}
}

func TestSessionRootValidationMatchesProtocolContract(t *testing.T) {
	tests := []struct {
		name       string
		cwd        string
		additional []string
		wantErr    bool
	}{
		{name: "absolute root", cwd: "/workspace"},
		{name: "relative root", cwd: "workspace", wantErr: true},
		{name: "relative additional root", cwd: "/workspace", additional: []string{"relative"}, wantErr: true},
		{name: "unsupported absolute additional root", cwd: "/workspace", additional: []string{"/other"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSessionRoots(test.cwd, test.additional)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, test.wantErr)
			}
		})
	}
}
