package justbash

import (
	"encoding/json"
	"testing"
)

func TestRequestAndResponseJSONContract(t *testing.T) {
	request, err := json.Marshal(Request{Command: "pwd", Cwd: "/workspace", TimeoutMilliseconds: 1250})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(request), `{"command":"pwd","cwd":"/workspace","timeout_milliseconds":1250}`; got != want {
		t.Fatalf("request JSON = %s, want %s", got, want)
	}

	response, err := json.Marshal(Response{Stdout: "/workspace\n", ExitCode: 0})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(response), `{"stdout":"/workspace\n","stderr":"","exit_code":0}`; got != want {
		t.Fatalf("response JSON = %s, want %s", got, want)
	}
}

func TestExecutorNameDefaultsAndValidation(t *testing.T) {
	if got := normalizeExecutorName(""); got != DefaultExecutorGlobal {
		t.Fatalf("default executor = %q", got)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("padded executor name was accepted")
		}
	}()
	normalizeExecutorName(" padded ")
}
