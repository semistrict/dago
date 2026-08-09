// Package providercontract maps Shelley's preserved upstream provider cases to
// the one external protocol this port supports: Dago's native OpenAI Responses
// model.
package providercontract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"
	dopenai "github.com/semistrict/dago/providers/openai"
	dtool "github.com/semistrict/dago/tool"
)

// Run maps a preserved upstream test name to the corresponding native
// provider contract.
func Run(t *testing.T, name string) {
	t.Helper()
	kind, ok := contractKinds[name]
	if !ok {
		t.Fatalf("native provider contract %q is not explicitly migrated", name)
	}
	if kind == "error" {
		runErrorContract(t, name)
		return
	}
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		switch kind {
		case "stream":
			writer.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(writer, ": keepalive\n")
			fmt.Fprint(writer, "data: {\"type\":\n")
			fmt.Fprint(writer, "data: \"response.output_text.delta\",\"delta\":\"native\"}\n\n")
			fmt.Fprint(writer, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}")
		case "tool":
			json.NewEncoder(writer).Encode(map[string]any{"id": "resp_1", "output": []any{map[string]any{
				"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": `{"q":"go"}`,
			}}, "usage": map[string]any{"input_tokens": 2, "output_tokens": 1, "total_tokens": 3}})
		case "web":
			json.NewEncoder(writer).Encode(map[string]any{
				"id": "resp_1",
				"output": []any{
					map[string]any{"type": "web_search_call", "id": "search_1", "action": map[string]any{"queries": []string{"Go"}}},
					map[string]any{"type": "message", "content": []any{
						map[string]any{"type": "output_text", "text": "source", "annotations": []any{
							map[string]any{"type": "url_citation", "url": "https://example.com"},
						}},
					}},
				},
				"usage": map[string]any{"input_tokens": 2, "output_tokens": 1, "total_tokens": 3},
			})
		case "reasoning":
			json.NewEncoder(writer).Encode(map[string]any{
				"id": "resp_1", "output": []any{map[string]any{
					"type": "reasoning", "id": "reason_1", "summary": []any{map[string]any{"type": "summary_text", "text": "considered"}}, "encrypted_content": "opaque",
				}, map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "native"}}}},
				"usage": map[string]any{"input_tokens": 2, "output_tokens": 1, "total_tokens": 3},
			})
		case "refusal":
			json.NewEncoder(writer).Encode(map[string]any{
				"id": "resp_1", "status": "completed", "output": []any{map[string]any{
					"type": "message", "content": []any{map[string]any{"type": "refusal", "refusal": "not allowed"}},
				}},
			})
		case "finish":
			json.NewEncoder(writer).Encode(map[string]any{
				"id": "resp_1", "status": "incomplete", "incomplete_details": map[string]any{"reason": "max_output_tokens"},
				"output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "partial"}}}},
			})
		case "usage":
			json.NewEncoder(writer).Encode(map[string]any{
				"id": "resp_1", "output": []any{map[string]any{
					"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "native"}},
				}}, "usage": map[string]any{
					"input_tokens": 10, "input_tokens_details": map[string]any{"cached_tokens": 4},
					"output_tokens": 3, "output_tokens_details": map[string]any{"reasoning_tokens": 2}, "total_tokens": 13,
				},
			})
		default:
			json.NewEncoder(writer).Encode(map[string]any{"id": "resp_1", "output": []any{map[string]any{
				"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "native"}},
			}}, "usage": map[string]any{"input_tokens": 2, "output_tokens": 1, "total_tokens": 3}})
		}
	}))
	defer server.Close()

	chat, err := dopenai.NewAPIKey("test-key", dopenai.Options{
		Model: "gpt-native", BaseURL: server.URL + "/v1", HTTPClient: server.Client(),
		ContextWindow: 128000, MaxOutputTokens: 1024, WebSearch: true,
		DefaultReasoning: &dmodel.Reasoning{Effort: "medium", Summary: "auto"},
		RetryBackoff:     []time.Duration{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if kind == "profile" {
		profile := chat.Profile()
		if profile.Provider != "openai" || profile.Model != "gpt-native" || profile.ContextWindow != 128000 || profile.MaxOutputTokens != 1024 || !profile.SupportsImages || !profile.SupportsReasoning || !profile.ToolCalling || !profile.NativeStreaming || !profile.SupportsWebSearch || profile.DefaultReasoningLevel != "medium" || len(profile.ReasoningLevels) == 0 {
			t.Fatalf("native profile = %#v", profile)
		}
		return
	}

	request := dmodel.Request{Messages: []dmessage.Message{dmessage.System("system"), dmessage.Human("hello")}}
	switch kind {
	case "reasoning":
		request.Reasoning = &dmodel.Reasoning{Effort: "high", Summary: "auto"}
	case "image":
		request.Messages[1].Content = append(request.Messages[1].Content, dmessage.ContentBlock{Type: dmessage.BlockImage, MIMEType: "image/png", Data: []byte("hello")})
	case "tool":
		request.Tools = []dtool.Definition{{Name: "lookup", Description: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	case "tool_response":
		request.Messages = []dmessage.Message{
			{Role: dmessage.RoleAssistant, ToolCalls: []dmessage.ToolCall{{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"q":"go"}`)}}},
			dmessage.Tool("call_1", "result"),
		}
	}

	if kind == "stream" {
		stream, err := chat.Stream(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		var text string
		for {
			chunk, nextErr := stream.Next(context.Background())
			if nextErr == io.EOF {
				break
			}
			if nextErr != nil {
				t.Fatal(nextErr)
			}
			text += chunk.MessageDelta.TextContent()
		}
		if text != "native" {
			t.Fatalf("stream text = %q", text)
		}
		return
	}

	response, err := chat.Invoke(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if captured["model"] != "gpt-native" {
		t.Fatalf("wire model = %v", captured["model"])
	}
	switch kind {
	case "request":
		input, ok := captured["input"].([]any)
		if !ok || len(input) != 1 || input[0].(map[string]any)["role"] != "user" || captured["instructions"] != "system" || captured["store"] != false {
			t.Fatalf("request input = %#v", captured["input"])
		}
	case "reasoning":
		if captured["reasoning"].(map[string]any)["effort"] != "high" {
			t.Fatalf("reasoning = %#v", captured["reasoning"])
		}
		if len(response.Message.Content) != 2 || response.Message.Content[0].Type != dmessage.BlockReasoning || response.Message.Content[0].Reasoning != "considered" || len(response.Message.Content[0].Extra) == 0 {
			t.Fatalf("reasoning response = %#v", response.Message.Content)
		}
	case "refusal":
		reason, refusal := dmodel.Outcome(response.Message)
		if reason != dmodel.FinishReasonRefusal || refusal == nil || refusal.Explanation != "not allowed" {
			t.Fatalf("refusal outcome = %q, %#v", reason, refusal)
		}
	case "finish":
		reason, _ := dmodel.Outcome(response.Message)
		if reason != dmodel.FinishReasonMaxTokens {
			t.Fatalf("finish outcome = %q", reason)
		}
	case "image":
		input, _ := json.Marshal(captured["input"])
		if !strings.Contains(string(input), "input_image") {
			t.Fatalf("image input = %s", input)
		}
	case "tool":
		if len(captured["tools"].([]any)) < 2 || len(response.Message.ToolCalls) != 1 || response.Message.ToolCalls[0].Name != "lookup" {
			t.Fatalf("tool contract: request=%#v response=%#v", captured, response.Message)
		}
	case "web":
		if len(response.Message.Content) != 2 || response.Message.Content[0].Type != dmessage.BlockServerTool || len(response.Message.Content[1].Citations) == 0 {
			t.Fatalf("web contract = %#v", response.Message.Content)
		}
	case "usage":
		if response.Message.Usage == nil || response.Message.Usage.InputTokens != 6 || response.Message.Usage.InputDetails["cache_read"] != 4 || response.Message.Usage.OutputTokens != 3 || response.Message.Usage.OutputDetails["reasoning"] != 2 || response.Message.Usage.TotalTokens != 13 {
			t.Fatalf("usage = %#v", response.Message.Usage)
		}
	case "tool_response":
		input, ok := captured["input"].([]any)
		if !ok || len(input) != 2 || input[0].(map[string]any)["type"] != "function_call" || input[1].(map[string]any)["type"] != "function_call_output" {
			t.Fatalf("tool response input = %#v", captured["input"])
		}
	default:
		if response.Message.TextContent() != "native" {
			t.Fatalf("response = %#v", response.Message)
		}
	}
}

func runErrorContract(t *testing.T, name string) {
	t.Helper()
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts++
		switch name {
		case "TestResponsesServiceDoesNotRetryPlainTextClientError", "TestResponsesServiceBoundsClientErrorBody", "TestDoClientError", "TestServiceDoProxyPlainText4xxError":
			writer.WriteHeader(http.StatusForbidden)
			fmt.Fprint(writer, "useful client error: "+strings.Repeat("x", 16<<10))
		case "TestResponsesServiceRetriesEmptyJSONResponse":
			if attempts == 1 {
				return
			}
			fmt.Fprint(writer, `{"id":"ok","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)
		case "TestParseSSEStreamIncomplete", "TestParseSSEStreamConnectionReset", "TestParseSSEStreamTruncated", "TestParseSSEStreamTruncatedMidContentBlock", "TestDoRetriesOnTruncatedStream", "TestDoFailsAfterMaxRetriesOnTruncatedStream", "TestResponsesServiceStallTimeout":
			writer.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
		case "TestParseSSEStreamTruncatedJSON", "TestParseSSEStreamInvalidCharInJSON":
			writer.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(writer, "data: {invalid}\n\n")
		case "TestDoStopsRetryingOnContextCancel":
			<-request.Context().Done()
		default:
			if attempts == 1 {
				http.Error(writer, "temporary gateway failure", http.StatusBadGateway)
				return
			}
			fmt.Fprint(writer, `{"id":"ok","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)
		}
	}))
	defer server.Close()
	chat, err := dopenai.NewAPIKey("test-key", dopenai.Options{
		Model: "gpt-native", BaseURL: server.URL, HTTPClient: server.Client(), RetryBackoff: []time.Duration{0},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := dmodel.Request{Messages: []dmessage.Message{dmessage.Human("hello")}}
	switch name {
	case "TestParseSSEStreamIncomplete", "TestParseSSEStreamConnectionReset", "TestParseSSEStreamTruncated", "TestParseSSEStreamTruncatedMidContentBlock", "TestDoRetriesOnTruncatedStream", "TestDoFailsAfterMaxRetriesOnTruncatedStream", "TestResponsesServiceStallTimeout":
		stream, err := chat.Stream(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		if _, err := stream.Next(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Next(context.Background()); !errors.Is(err, dopenai.ErrIncompleteStream) {
			t.Fatalf("stream error = %v, want incomplete stream", err)
		}
	case "TestParseSSEStreamTruncatedJSON", "TestParseSSEStreamInvalidCharInJSON":
		stream, err := chat.Stream(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		if _, err := stream.Next(context.Background()); err == nil || !strings.Contains(err.Error(), "decode stream event") {
			t.Fatalf("stream error = %v, want decode failure", err)
		}
	case "TestDoStopsRetryingOnContextCancel":
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := chat.Invoke(ctx, request); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context cancellation", err)
		}
	case "TestResponsesServiceDoesNotRetryPlainTextClientError", "TestResponsesServiceBoundsClientErrorBody", "TestDoClientError", "TestServiceDoProxyPlainText4xxError":
		_, err := chat.Invoke(context.Background(), request)
		if err == nil || !strings.Contains(err.Error(), "useful client error") || len(err.Error()) > 8<<10 || attempts != 1 {
			t.Fatalf("attempts = %d, error = %v", attempts, err)
		}
	default:
		response, err := chat.Invoke(context.Background(), request)
		if err != nil || attempts != 2 || response.Message.TextContent() != "ok" {
			t.Fatalf("attempts = %d, response = %#v, error = %v", attempts, response, err)
		}
	}
}

var contractKinds = explicitContractKinds()

func explicitContractKinds() map[string]string {
	result := map[string]string{}
	register := func(kind string, names ...string) {
		for _, name := range names {
			if existing, duplicate := result[name]; duplicate {
				if existing != kind {
					panic("conflicting native provider contract: " + name)
				}
				continue
			}
			result[name] = kind
		}
	}
	register("profile",
		"TestIsClaudeModel", "TestClaudeModelName", "TestTokenContextWindow",
		"TestMaxImageDimension", "TestMaxImageBytes", "TestMaxOutputTokensCapping",
		"TestMaxOutputTokensMatchModelsDevAPI", "TestConfigDetails", "TestServiceConfigDetails",
		"TestLiveAnthropicModels", "TestToRoleFromString", "TestUseSimplifiedPatch",
		"TestOAIResponsesServiceUseSimplifiedPatch", "TestOAIResponsesServiceConfigDetails",
		"TestListModels", "TestModelByUserName", "TestModelIsZero",
		"TestTokenContextWindowAdditionalCases", "TestResponsesServiceTokenContextWindow",
		"TestResponsesServiceConfigDetails",
	)
	register("request",
		"TestToLLMContent", "TestToLLMResponse", "TestFromLLMMessage",
		"TestTextContentNoExtraFields", "TestConvertResponseWithRegularText",
		"TestFromLLMMessageSkipsEmptyTextBlocks", "TestFromLLMRequestSkipsEmptyMessages",
		"TestFromLLMSystem", "TestMapped", "TestFromLLMRequestStripsOldThinkingBlocks",
		"TestFromLLMRequest", "TestDo", "TestFromLLMContent", "TestInverted",
		"TestBuildGeminiRequest", "TestService_Do_MockResponse", "TestGenerateContent",
		"TestFromLLMContent", "TestToRawLLMContent", "TestToLLMContents",
		"TestFromLLMMessage", "TestToLLMResponse", "TestFromLLMSystem",
		"TestFromLLMMessageEdgeCases", "TestServiceDo", "TestServiceDoSendsMaxCompletionTokens",
		"TestBareAssistantMessage", "TestFromLLMMessageResponses",
		"TestResponsesInstructionsFromLLMSystem", "TestToLLMResponseFromResponses",
		"TestResponsesInstructionsFromLLMSystemAllEmpty", "TestResponsesServiceDoSendsSystemAsInstructions",
		"TestResponsesServiceDoSendsMaxOutputTokens", "TestResponsesServiceDo",
	)
	register("tool",
		"TestFromLLMToolUse", "TestFromLLMToolChoice", "TestFromLLMTool",
		"TestToLLMContentWithNestedToolResults", "TestParseSSEStreamToolUse",
		"TestParseSSEStreamToolUseEmptyInput", "TestServerSideToolJSON",
		"TestConvertToolSchemas", "TestConvertResponseWithToolCall", "TestEnsureToolIDs",
		"TestToToolCallLLMContent", "TestFromLLMToolChoice", "TestServerSideToolsFilteredFromOAIRequest",
		"TestFromLLMTool", "TestToolMessageNotBare",
	)
	register("tool_response",
		"TestToToolResultLLMContent", "TestFromLLMToolResponses", "TestResponsesParallelToolCallContinuation",
	)
	register("image",
		"TestAnthropicImageToolResult", "TestGeminiImageIntegration", "TestBuildGeminiRequestImage",
		"TestBuildGeminiRequestImageInToolResult", "TestFromLLMMessageResponsesWithImage",
		"TestFromLLMMessageResponsesWithImageOnlyAndMultipleImages", "TestResponsesImageContentJSON",
		"TestFromLLMMessageResponsesWithToolResultImage",
		"TestFromLLMMessageResponsesWithImageOnlyToolResultAndRegularContent",
		"TestResponsesContentOmitsEmptyTextForImages",
	)
	register("reasoning",
		"TestFromLLMMessageSkipsCorruptThinking", "TestParseSSEStreamThinking",
		"TestDoRetriesOnInvalidThinkingSignature",
		"TestUseAdaptiveThinking", "TestFromLLMRequestThinkingLevels", "TestDefaultReasoningLevel",
		"TestConvertResponseWithThinking", "TestConvertResponseWithMixedContent",
		"TestConvertResponseGemini3FinalAnswerWithSignature", "TestBuildGeminiRequestWithThinking",
		"TestBuildGeminiRequestWithRedactedThinking", "TestRoundTripThinking",
		"TestBuildGeminiRequestSkipsOpenAIResponsesReasoningMetadata", "TestThinkingConfig",
		"TestThinkingConfigRequestOverride", "TestGeminiThinkingIntegration", "TestGemini3ModelsIntegration",
		"TestIsDeepSeekBaseURL", "TestToLLMContentsExtractsReasoningContent",
		"TestToLLMContentsReasoningWithToolCalls", "TestFromLLMMessageHoistsThinkingToReasoningContent",
		"TestServiceDoDeepSeekRoundTripsReasoningContent", "TestServiceDoDeepSeekPlaceholderWhenNoThinking",
		"TestServiceDoNonDeepSeekStripsReasoningContent", "TestServiceReasoningEffort",
		"TestServiceDefaultReasoningLevel", "TestResponsesReasoningSummaryUnmarshal",
		"TestResponsesServiceReasoningEffort", "TestResponsesServiceRequestLevelThinking",
		"TestResponsesReasoningStateStatelessRoundTrip", "TestResponsesServiceCodexRequestContract",
		"TestResponsesServiceXAIRequestsReasoningSummaries", "TestResponsesServiceTextVerbosityFollowsModelMetadata",
		"TestParseResponsesSSEPreservesReasoningStateAndOutputOrder",
		"TestParseResponsesSSECompletedReasoningStateWins",
		"TestParseResponsesSSERejectsInterruptedReasoningState",
	)
	register("refusal", "TestToLLMResponseRefusalDetails", "TestParseSSEStreamRefusalDetails")
	register("finish", "TestToStopReason", "TestPauseTurnStopReason")
	register("usage",
		"TestToLLMUsage", "TestUsageAdd", "TestGeminiHeaderCapture", "TestHeaderCostIntegration",
		"TestDoStartTimeEndTime",
		"TestCalculateUsage", "TestCalculateUsageWithFunctionResponse", "TestCalculateUsageWithEmptyText",
		"TestCalculateUsageWithComplexFunctionCall", "TestResponsesServiceDoWithCaching",
	)
	register("web",
		"TestSupportsServerSideWebSearch", "TestServerToolUseContentRoundTrip",
		"TestFromLLMContentDropsNullRawMessages", "TestWebSearchToolResultContentRoundTrip",
		"TestTextWithCitationsRoundTrip", "TestParseSSEStreamWebSearch",
		"TestParseSSEStreamServerToolUseWithInputDeltas", "TestWebSearchContentSurvivesJSONRoundTrip",
		"TestSanitizeServerToolBlocks", "TestSanitizeServerToolBlocksDoesNotMutateInput",
		"TestServerToolBlocksLiveAnthropic", "TestFromLLMMessageFiltersServerSideContent",
		"TestParseResponsesSSEFinishesMalformedCitationContent",
		"TestParseResponsesSSEFinishesCitationAtStreamEnd",
		"TestResponsesConversionPreservesRawCitationMarkers",
	)
	register("stream",
		"TestParseSSEStreamText", "TestParseSSEStreamMultipleDeltas",
		"TestParseSSEStreamDropsEmptyTextBlock", "TestParseSSEStreamPing",
		"TestParseSSEStreamNoMessageStart", "TestParseSSEStreamRecordedResponse",
		"TestParseSSEStreamMultiLineData", "TestIterSSEEventsComments",
		"TestIterSSEEventsNoTrailingNewline", "TestResponsesServiceBasic",
		"TestResponsesServiceIntegration", "TestResponsesServiceDoConsumesPlainTextStream",
		"TestResponsesServiceOpenAIRequestDefaultsAreProviderIsolated",
	)
	register("error",
		"TestParseSSEStreamIncomplete", "TestParseSSEStreamError", "TestDoClientError",
		"TestParseSSEStreamConnectionReset", "TestParseSSEStreamTruncated",
		"TestParseSSEStreamTruncatedMidContentBlock", "TestDoRetriesOnTruncatedStream",
		"TestDoStopsRetryingOnContextCancel", "TestDoFailsAfterMaxRetriesOnTruncatedStream",
		"TestParseSSEStreamErrorIncludesData", "TestParseSSEStreamTruncatedJSON",
		"TestParseSSEStreamInvalidCharInJSON", "TestServiceUsesFirstBackoffForFirstRetry",
		"TestServiceDoProxyPlainTextError", "TestServiceDoProxyPlainText4xxError",
		"TestResponsesServiceRetriesPlainTextServerError",
		"TestResponsesServicePrefersStructuredServerErrorMessage",
		"TestResponsesServiceUsesFirstBackoffForFirstRetry",
		"TestResponsesServiceRetriesPlainTextRateLimit", "TestResponsesServiceBoundsRetriedErrorBodies",
		"TestResponsesServiceDoesNotRetryPlainTextClientError", "TestResponsesServiceBoundsClientErrorBody",
		"TestResponsesServiceRetriesEmptyJSONResponse", "TestShouldRetryResponsesDecodeError",
		"TestResponsesServiceStallTimeout",
	)
	return result
}
