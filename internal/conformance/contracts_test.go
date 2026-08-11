package conformance

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/datool"
)

func TestGeneratedContractsDecodeIntoPublicTypes(t *testing.T) {
	data, err := os.ReadFile("testdata/contracts.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Generated bool `json:"generated"`
		Contract  struct {
			Message       damessage.Message       `json:"message"`
			Tool          datool.Definition       `json:"tool"`
			ModelResponse damodel.Response        `json:"model_response"`
			Checkpoint    dacheckpoint.Checkpoint `json:"checkpoint"`
		} `json:"contract"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.Generated || envelope.Contract.Message.Role != damessage.RoleAssistant || envelope.Contract.Tool.Name != "lookup" || envelope.Contract.Checkpoint.Version != dacheckpoint.LatestVersion {
		t.Fatalf("fixture = %#v", envelope)
	}
	if err := envelope.Contract.Message.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := envelope.Contract.Tool.Validate(); err != nil {
		t.Fatal(err)
	}
}
