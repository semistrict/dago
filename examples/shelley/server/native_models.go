package server

import "github.com/semistrict/dago/damodel"

func nativeChatFor(provider LLMProvider, modelID string) (damodel.Chat, error) {
	return provider.GetChat(modelID)
}

type nativeModelProvider struct{ LLMProvider }

func (provider nativeModelProvider) GetChat(modelID string) (damodel.Chat, error) {
	return nativeChatFor(provider.LLMProvider, modelID)
}
