package models

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"
)

func TestBuiltInLunaExposesNativeDagoChat(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(writer, `{"id":"r","output":[{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"native"}]}]}`)
	}))
	defer server.Close()

	catalog := ByID("gpt-5.6-luna")
	if catalog == nil {
		t.Fatal("Luna is missing from the built-in catalog")
	}
	chat, err := catalog.Build(server.URL, "secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	response, err := chat.Invoke(context.Background(), dmodel.Request{
		Messages: []dmessage.Message{dmessage.Human("hello")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.TextContent() != "native" {
		t.Fatalf("response = %#v", response)
	}
	reasoning, ok := requestBody["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "medium" || reasoning["summary"] != "auto" {
		t.Fatalf("default reasoning = %#v", requestBody["reasoning"])
	}
}

func TestNativeUsageProjectsCachedTokens(t *testing.T) {
	got := legacyUsage(&dmessage.Usage{
		InputTokens: 20, OutputTokens: 5, TotalTokens: 105,
		InputDetails: map[string]int{"cache_read": 80},
	}, "gpt-5.6-luna", "openai")
	if got.InputTokens != 20 || got.CacheReadInputTokens != 80 || got.OutputTokens != 5 {
		t.Fatalf("projected usage = %#v", got)
	}
}
