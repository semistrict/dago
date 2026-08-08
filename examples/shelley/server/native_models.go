package server

import (
	"fmt"

	dmodel "github.com/semistrict/dago/model"

	"shelley.exe.dev/llm"
)

func nativeChatFor(provider LLMProvider, modelID string) (dmodel.Chat, error) {
	if native, ok := provider.(interface {
		GetChat(string) (dmodel.Chat, error)
	}); ok {
		return native.GetChat(modelID)
	}
	service, err := provider.GetService(modelID)
	if err != nil {
		return nil, err
	}
	native, ok := service.(interface{ DagoChat() dmodel.Chat })
	if !ok || native.DagoChat() == nil {
		return nil, fmt.Errorf("model %s does not provide a native chat implementation", modelID)
	}
	return native.DagoChat(), nil
}

type nativeModelProvider struct{ LLMProvider }

func serviceHasNativeChat(service llm.Service) bool {
	if policy, ok := service.(interface{ UseDagoChatInAgent() bool }); ok && !policy.UseDagoChatInAgent() {
		return false
	}
	native, ok := service.(interface{ DagoChat() dmodel.Chat })
	return ok && native.DagoChat() != nil
}

func (provider nativeModelProvider) GetChat(modelID string) (dmodel.Chat, error) {
	return nativeChatFor(provider.LLMProvider, modelID)
}
