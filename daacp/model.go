package daacp

import acp "github.com/coder/acp-go-sdk"

const (
	// ModelListMethod returns the model selector advertised by this ACP agent
	// without creating a session.
	ModelListMethod = "_dago/models/list"
)

// ModelListRequest is the versioned ACP model-discovery request.
type ModelListRequest struct {
	Version int `json:"version"`
}

// ModelInfo is one selectable model advertised by the ACP agent.
type ModelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ModelListResponse is the versioned ACP model-discovery response.
type ModelListResponse struct {
	Version      int         `json:"version"`
	DefaultModel string      `json:"default_model"`
	Models       []ModelInfo `json:"models"`
}

func modelListResponse(options []acp.SessionConfigOption) ModelListResponse {
	response := ModelListResponse{Version: 1, Models: []ModelInfo{}}
	for _, option := range options {
		if option.Select == nil || option.Select.Id != modelConfigID {
			continue
		}
		response.DefaultModel = string(option.Select.CurrentValue)
		seen := make(map[string]bool)
		appendOption := func(candidate acp.SessionConfigSelectOption) {
			id := string(candidate.Value)
			if id == "" || candidate.Name == "" || seen[id] {
				return
			}
			seen[id] = true
			response.Models = append(response.Models, ModelInfo{ID: id, Name: candidate.Name})
		}
		if option.Select.Options.Ungrouped != nil {
			for _, candidate := range *option.Select.Options.Ungrouped {
				appendOption(candidate)
			}
		}
		if option.Select.Options.Grouped != nil {
			for _, group := range *option.Select.Options.Grouped {
				for _, candidate := range group.Options {
					appendOption(candidate)
				}
			}
		}
		break
	}
	return response
}
