package conformance

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/semistrict/dago/checkpoint"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/tool"
)

func TestGeneratedContractsDecodeIntoPublicTypes(t *testing.T) {
	data, err := os.ReadFile("testdata/contracts.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Generated bool `json:"generated"`
		Contract  struct {
			Message       message.Message       `json:"message"`
			Tool          tool.Definition       `json:"tool"`
			ModelResponse model.Response        `json:"model_response"`
			Checkpoint    checkpoint.Checkpoint `json:"checkpoint"`
		} `json:"contract"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.Generated || envelope.Contract.Message.Role != message.RoleAssistant || envelope.Contract.Tool.Name != "lookup" || envelope.Contract.Checkpoint.Version != checkpoint.LatestVersion {
		t.Fatalf("fixture = %#v", envelope)
	}
	if err := envelope.Contract.Message.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := envelope.Contract.Tool.Validate(); err != nil {
		t.Fatal(err)
	}
}
