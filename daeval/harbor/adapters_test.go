package harbor_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/semistrict/dago/daeval/harbor"
)

func TestContextBenchAdapterPreservesPinnedPromptAndSafeMetadata(t *testing.T) {
	t.Parallel()

	record := harbor.NewContextBenchRecord("cb-cloud-17", "Which resident owns the most vehicles?")
	record.Difficulty = "easy"
	record.QuestionType = "comparison_tiebreak"
	task, err := record.Adapt()
	if err != nil {
		t.Fatal(err)
	}
	wantInstruction := "Which resident owns the most vehicles?\n\nUse only the files under `/app/files`. Write your final answer (and nothing else) to `/app/answer.txt`.\n"
	if task.Instruction != wantInstruction {
		t.Fatalf("instruction = %q", task.Instruction)
	}
	if task.Name != "cb-cloud-17" || task.Category != "context" {
		t.Fatalf("task identity = %#v", task)
	}
	wantMetadata := map[string]string{
		"source":            "contextbench",
		"suite":             "cloud",
		"line_index":        "17",
		"difficulty":        "easy",
		"source_difficulty": "easy",
		"question_type":     "comparison_tiebreak",
		"output_path":       "/app/answer.txt",
		"network_mode":      "allowlist",
	}
	assertStringMap(t, task.Metadata, wantMetadata)
	if strings.Contains(strings.ToLower(task.Instruction), "ground_truth") {
		t.Fatalf("instruction exposes verifier data: %q", task.Instruction)
	}
}

func TestContextBenchDefaultsAndOrderedBatchCancellation(t *testing.T) {
	t.Parallel()

	first := harbor.NewContextBenchRecord("cb-cloud-2", "first")
	second := harbor.NewContextBenchRecord("cb-cloud-1", "second")
	tasks, err := harbor.AdaptContextBench(context.Background(), first, second)
	if err != nil {
		t.Fatal(err)
	}
	if tasks[0].Name != "cb-cloud-2" || tasks[1].Name != "cb-cloud-1" {
		t.Fatalf("adapter reordered tasks: %#v", tasks)
	}
	if tasks[0].Metadata["difficulty"] != "unclassified" || tasks[0].Metadata["question_type"] != "unclassified" {
		t.Fatalf("defaults = %#v", tasks[0].Metadata)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := harbor.AdaptContextBench(ctx, first); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestDRBenchAdapterBuildsAppModeResearchTaskWithoutVerifierData(t *testing.T) {
	t.Parallel()

	record := harbor.NewDRBenchRecord(
		"DR0001",
		"How should Acme respond to the new rules?",
		harbor.DRBenchNextcloud,
		harbor.DRBenchEmail,
		harbor.DRBenchNextcloud,
	)
	record.Date = "2025-08-27"
	record.Company = harbor.DRBenchCompany{
		Name:                   "Acme",
		Industry:               "Retail",
		Headquarters:           "Boston",
		KeyProductsAndServices: []string{"widgets", "support"},
	}
	record.Persona = harbor.DRBenchPersona{Name: "Dana Ray", Role: "Compliance Lead"}
	record.AppUsers = map[harbor.DRBenchApp]string{
		harbor.DRBenchNextcloud: "dana.ray",
		harbor.DRBenchEmail:     "dana@example.com",
	}
	record.PersonaCredentials = true
	record.Difficulty = "easy"
	record.Industry = "retail"
	record.Domain = "compliance"
	record.DocumentCount = 2
	record.InsightCount = 3
	record.ExternalInsightCount = 1
	record.DistractorCount = 1

	task, err := record.Adapt()
	if err != nil {
		t.Fatal(err)
	}
	if task.Name != "DR0001" || task.Category != "research" {
		t.Fatalf("task identity = %#v", task)
	}
	if task.Metadata["apps"] != "email,nextcloud" {
		t.Fatalf("stable apps = %q", task.Metadata["apps"])
	}
	if task.Metadata["credential_regime"] != "persona" ||
		task.Metadata["upstream_revision"] != harbor.DRBenchUpstreamRevision ||
		task.Metadata["insight_count"] != "3" ||
		task.Metadata["external_insight_count"] != "1" ||
		task.Metadata["distractor_count"] != "1" {
		t.Fatalf("metadata = %#v", task.Metadata)
	}
	for _, text := range []string{
		"How should Acme respond",
		"Dana Ray",
		"Compliance Lead",
		"Acme",
		"Boston",
		"dana.ray",
		"dana@example.com",
		"http://drbench:8081",
		"drbench:1143",
		"extract-text",
		"web_search",
		"/app/report.md",
		"RoundCube-",
		"MatterMost-",
		"subject exactly",
	} {
		if !strings.Contains(task.Instruction, text) {
			t.Fatalf("instruction misses %q:\n%s", text, task.Instruction)
		}
	}
	if strings.Contains(task.Instruction, "http://drbench:8082") {
		t.Fatalf("unused Mattermost app appeared:\n%s", task.Instruction)
	}
	if strings.Contains(task.Instruction, "%!") {
		t.Fatalf("prompt contains a formatting artifact:\n%s", task.Instruction)
	}
	for _, forbidden := range []string{"ground truth", "gold insight", "distractor answer", "admin_pwd", "current_user_pwd"} {
		if strings.Contains(strings.ToLower(task.Instruction), forbidden) {
			t.Fatalf("instruction exposes %q:\n%s", forbidden, task.Instruction)
		}
	}

	again, err := record.Adapt()
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(again)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("adapter is nondeterministic:\n%s\n%s", firstJSON, secondJSON)
	}
}

func TestDRBenchUsefulDefaultProfilesAndUsers(t *testing.T) {
	t.Parallel()

	record := harbor.NewDRBenchRecord(
		"SANITY0",
		"Summarize the available evidence.",
		harbor.DRBenchMattermost,
		harbor.DRBenchFileSystem,
		harbor.DRBenchEmail,
		harbor.DRBenchNextcloud,
	)
	task, err := record.Adapt()
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Unknown", "admin@drbench.com", "current.user", "log in as `admin`"} {
		if !strings.Contains(task.Instruction, text) {
			t.Fatalf("default prompt misses %q:\n%s", text, task.Instruction)
		}
	}
	if task.Metadata["difficulty"] != "unclassified" ||
		task.Metadata["industry"] != "unclassified" ||
		task.Metadata["domain"] != "unclassified" ||
		task.Metadata["credential_regime"] != "default" ||
		task.Metadata["network_mode"] != "public" {
		t.Fatalf("defaults = %#v", task.Metadata)
	}
}

func TestAdaptersRejectMalformedOrOversizedRecords(t *testing.T) {
	t.Parallel()

	for _, call := range []func(){
		func() { harbor.NewContextBenchRecord("../cb-cloud-1", "question") },
		func() { harbor.NewContextBenchRecord("cb-cloud-1", " ") },
		func() { harbor.NewDRBenchRecord("dr0001", "question", harbor.DRBenchEmail) },
		func() { harbor.NewDRBenchRecord("DR0001", "question", harbor.DRBenchApp("dropbox")) },
	} {
		assertPanics(t, call)
	}
	negative := harbor.NewDRBenchRecord("DR0001", "question", harbor.DRBenchEmail)
	negative.DocumentCount = -1
	if _, err := negative.Adapt(); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("negative count error = %v", err)
	}
	impossible := harbor.NewDRBenchRecord("DR0001", "question", harbor.DRBenchEmail)
	impossible.InsightCount = 1
	impossible.ExternalInsightCount = 2
	if _, err := impossible.Adapt(); err == nil || !strings.Contains(err.Error(), "exceeds total") {
		t.Fatalf("impossible count error = %v", err)
	}
	hostileUser := harbor.NewDRBenchRecord("DR0001", "question", harbor.DRBenchEmail)
	hostileUser.AppUsers = map[harbor.DRBenchApp]string{harbor.DRBenchEmail: "user\ninjected"}
	if _, err := hostileUser.Adapt(); err == nil || !strings.Contains(err.Error(), "unsafe form") {
		t.Fatalf("hostile username error = %v", err)
	}
	oversized := harbor.NewContextBenchRecord("cb-cloud-1", strings.Repeat("x", 513<<10))
	if _, err := oversized.Adapt(); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized record error = %v", err)
	}
}

func TestAdapterBatchWorkBoundAndHarnessIntegration(t *testing.T) {
	t.Parallel()

	records := make([]harbor.DRBenchRecord, 1_001)
	for index := range records {
		records[index] = harbor.NewDRBenchRecord("DR0001", "question", harbor.DRBenchEmail)
	}
	if _, err := harbor.AdaptDRBench(context.Background(), records...); err == nil || !strings.Contains(err.Error(), "exceeds 1000") {
		t.Fatalf("batch bound error = %v", err)
	}

	contextTask, err := harbor.NewContextBenchRecord("cb-cloud-1", "find the answer").Adapt()
	if err != nil {
		t.Fatal(err)
	}
	researchTask, err := harbor.NewDRBenchRecord("DR0001", "research the answer", harbor.DRBenchEmail).Adapt()
	if err != nil {
		t.Fatal(err)
	}
	reward := 1.0
	runner := harbor.RunnerFunc(func(_ context.Context, task harbor.Task) (harbor.Trial, error) {
		if task.Metadata["source"] != "contextbench" && task.Metadata["source"] != "drbench" {
			t.Fatalf("unexpected adapter source: %#v", task.Metadata)
		}
		return harbor.Trial{Reward: &reward}, nil
	})
	report := harbor.NewBenchmark(runner).Evaluate(context.Background(), contextTask, researchTask)
	if report.Evaluation.Passed != 2 || report.Evaluation.CategoryScores["context"] != 1 || report.Evaluation.CategoryScores["research"] != 1 {
		t.Fatalf("integrated report = %#v", report.Evaluation)
	}
}

func assertStringMap(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("map = %#v, want %#v", got, want)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("map[%q] = %q, want %q", key, got[key], value)
		}
	}
}
