package server

import dmodel "github.com/semistrict/dago/model"

func nativeChatFor(provider LLMProvider, modelID string) (dmodel.Chat, error) {
	return provider.GetChat(modelID)
}

type nativeModelProvider struct{ LLMProvider }

func (provider nativeModelProvider) GetChat(modelID string) (dmodel.Chat, error) {
	return nativeChatFor(provider.LLMProvider, modelID)
}
