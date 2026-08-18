package harbor

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxAdapterTasks       = 1_000
	maxAdapterTextBytes   = 512 << 10
	maxAdapterListEntries = 64

	// DRBenchUpstreamRevision identifies the task-config revision used by the
	// pinned Deep Agents adapter. Dataset acquisition remains caller-owned.
	DRBenchUpstreamRevision = "0d699ecf6aa96b1de378595b432e9b16a82f0ed9"
)

var (
	contextBenchIDPattern  = regexp.MustCompile(`^cb-([a-z0-9]+)-([0-9]+)$`)
	drBenchIDPattern       = regexp.MustCompile(`^(?:DR[0-9]{4}|SANITY[0-9]+)$`)
	drBenchUsernamePattern = regexp.MustCompile(`^[A-Za-z0-9@._+-]{1,254}$`)
)

// ContextBenchRecord contains only agent-visible ContextBench fields. Ground
// truth deliberately has no representation here and stays with the verifier.
type ContextBenchRecord struct {
	taskID   string
	question string

	Difficulty   string
	QuestionType string
}

// NewContextBenchRecord constructs a record from its required task ID and
// question. Invalid static inputs panic; optional labels may be set afterward.
func NewContextBenchRecord(taskID, question string) ContextBenchRecord {
	if _, _, err := parseContextBenchID(taskID); err != nil {
		panic("context benchmark task ID must match cb-<suite>-<index>")
	}
	if strings.TrimSpace(question) == "" {
		panic("context benchmark question is required")
	}
	return ContextBenchRecord{taskID: taskID, question: question}
}

// Adapt converts a ContextBench record into an agent-visible Harbor task.
func (record ContextBenchRecord) Adapt() (Task, error) {
	suite, index, err := parseContextBenchID(record.taskID)
	if err != nil {
		return Task{}, err
	}
	if err := validateAdapterText(record.question, record.Difficulty, record.QuestionType); err != nil {
		return Task{}, fmt.Errorf("adapt ContextBench %q: %w", record.taskID, err)
	}
	if strings.TrimSpace(record.question) == "" {
		return Task{}, fmt.Errorf("adapt ContextBench %q: question is required", record.taskID)
	}
	difficulty := defaultLabel(record.Difficulty)
	questionType := defaultLabel(record.QuestionType)
	task := NewTask(record.taskID, record.question+"\n\nUse only the files under `/app/files`. Write your final answer (and nothing else) to `/app/answer.txt`.\n")
	task.Category = "context"
	task.Metadata = map[string]string{
		"source":            "contextbench",
		"suite":             suite,
		"line_index":        strconv.Itoa(index),
		"difficulty":        difficulty,
		"source_difficulty": difficulty,
		"question_type":     questionType,
		"output_path":       "/app/answer.txt",
		"network_mode":      "allowlist",
	}
	return task, nil
}

// AdaptContextBench converts records in declaration order. The operation is
// network-free, cancellation-aware, and bounded to 1,000 records per call.
func AdaptContextBench(ctx context.Context, records ...ContextBenchRecord) ([]Task, error) {
	if len(records) > maxAdapterTasks {
		return nil, fmt.Errorf("ContextBench adapter exceeds %d records", maxAdapterTasks)
	}
	tasks := make([]Task, 0, len(records))
	for index, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		task, err := record.Adapt()
		if err != nil {
			return nil, fmt.Errorf("ContextBench record %d: %w", index+1, err)
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

// DRBenchApp is one application exposed by a DRBench task image.
type DRBenchApp string

const (
	DRBenchNextcloud  DRBenchApp = "nextcloud"
	DRBenchMattermost DRBenchApp = "mattermost"
	DRBenchEmail      DRBenchApp = "email"
	DRBenchFileSystem DRBenchApp = "file_system"
)

var drBenchAppDetails = map[DRBenchApp]struct {
	defaultUser string
	guidance    string
}{
	DRBenchNextcloud: {
		defaultUser: "admin",
		guidance:    "the company cloud drive. Use HTTP Basic authentication over WebDAV. List one level with `curl -u \"$DRBENCH_NEXTCLOUD_USER:$DRBENCH_NEXTCLOUD_PASS\" -X PROPFIND -H 'Depth: 1' -H 'Host: localhost' http://drbench:8081/remote.php/dav/files/%s/`, then walk returned directories and GET files. The Depth and Host headers are required.",
	},
	DRBenchMattermost: {
		defaultUser: "admin@drbench.com",
		guidance:    "team chat. POST `http://drbench:8082/api/v4/users/login` with `login_id` and `password`; use the returned Token header as a bearer token.",
	},
	DRBenchEmail: {
		defaultUser: "current.user",
		guidance:    "the mailbox over IMAP at `drbench:1143`; Python's `imaplib` works well. The same mail is browsable at `http://drbench:8085`.",
	},
	DRBenchFileSystem: {
		defaultUser: "admin",
		guidance:    "local and shared drives exposed through a file browser at `http://drbench:8090`.",
	},
}

// DRBenchCompany is the agent-visible company profile.
type DRBenchCompany struct {
	Name                     string
	Industry                 string
	Headquarters             string
	Size                     string
	Employees                string
	AnnualRevenue            string
	MarketPosition           string
	Description              string
	KeyProductsAndServices   []string
	TargetMarkets            []string
	ComplianceCertifications []string
}

// DRBenchPersona is the agent-visible requester profile. It contains no
// password. AppUsers on DRBenchRecord selects usable usernames.
type DRBenchPersona struct {
	Name             string
	Role             string
	Department       string
	Seniority        string
	Email            string
	Responsibilities string
}

// DRBenchRecord contains one agent-visible deep-research request. Gold insights,
// distractors, verifier prompts, and passwords are intentionally absent.
type DRBenchRecord struct {
	taskID   string
	question string
	apps     []DRBenchApp

	Date                 string
	Company              DRBenchCompany
	Persona              DRBenchPersona
	AppUsers             map[DRBenchApp]string
	Difficulty           string
	Industry             string
	Domain               string
	DocumentCount        int
	InsightCount         int
	ExternalInsightCount int
	DistractorCount      int
	PersonaCredentials   bool
}

// NewDRBenchRecord constructs a record from its required ID, question, and at
// least one application. Invalid static inputs panic. Additional applications
// are optional; the manifest is de-duplicated and sorted.
func NewDRBenchRecord(taskID, question string, app DRBenchApp, additionalApps ...DRBenchApp) DRBenchRecord {
	if len(taskID) > 256 || !drBenchIDPattern.MatchString(taskID) {
		panic("DRBench task ID must be DR<four digits> or SANITY<digits>")
	}
	if strings.TrimSpace(question) == "" {
		panic("DRBench question is required")
	}
	apps, err := normalizeDRBenchApps(append([]DRBenchApp{app}, additionalApps...))
	if err != nil {
		panic(err)
	}
	return DRBenchRecord{taskID: taskID, question: question, apps: apps}
}

// Adapt converts a DRBench record into an agent-visible Harbor task.
func (record DRBenchRecord) Adapt() (Task, error) {
	if len(record.taskID) > 256 || !drBenchIDPattern.MatchString(record.taskID) {
		return Task{}, fmt.Errorf("DRBench task ID must be DR<four digits> or SANITY<digits>")
	}
	if strings.TrimSpace(record.question) == "" {
		return Task{}, fmt.Errorf("adapt DRBench %q: question is required", record.taskID)
	}
	apps, err := normalizeDRBenchApps(record.apps)
	if err != nil {
		return Task{}, fmt.Errorf("adapt DRBench %q: %w", record.taskID, err)
	}
	if err := validateDRBenchRecord(record); err != nil {
		return Task{}, fmt.Errorf("adapt DRBench %q: %w", record.taskID, err)
	}
	users, err := drBenchUsers(record, apps)
	if err != nil {
		return Task{}, fmt.Errorf("adapt DRBench %q: %w", record.taskID, err)
	}

	instruction := drBenchInstruction(record, apps, users)
	if len(instruction) > maxAdapterTextBytes {
		return Task{}, fmt.Errorf("adapt DRBench %q: rendered instruction exceeds %d bytes", record.taskID, maxAdapterTextBytes)
	}
	task := NewTask(record.taskID, instruction)
	task.Category = "research"
	regime := "default"
	if record.PersonaCredentials {
		regime = "persona"
	}
	appNames := make([]string, len(apps))
	for index, app := range apps {
		appNames[index] = string(app)
	}
	task.Metadata = map[string]string{
		"source":                 "drbench",
		"mode":                   "app",
		"task_id":                record.taskID,
		"upstream_revision":      DRBenchUpstreamRevision,
		"industry":               defaultLabel(record.Industry),
		"domain":                 defaultLabel(record.Domain),
		"difficulty":             defaultLabel(record.Difficulty),
		"credential_regime":      regime,
		"insight_count":          strconv.Itoa(record.InsightCount),
		"external_insight_count": strconv.Itoa(record.ExternalInsightCount),
		"distractor_count":       strconv.Itoa(record.DistractorCount),
		"document_count":         strconv.Itoa(record.DocumentCount),
		"apps":                   strings.Join(appNames, ","),
		"output_path":            "/app/report.md",
		"network_mode":           "public",
	}
	return task, nil
}

// AdaptDRBench converts records in declaration order. The operation is
// network-free, cancellation-aware, and bounded to 1,000 records per call.
func AdaptDRBench(ctx context.Context, records ...DRBenchRecord) ([]Task, error) {
	if len(records) > maxAdapterTasks {
		return nil, fmt.Errorf("DRBench adapter exceeds %d records", maxAdapterTasks)
	}
	tasks := make([]Task, 0, len(records))
	for index, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		task, err := record.Adapt()
		if err != nil {
			return nil, fmt.Errorf("DRBench record %d: %w", index+1, err)
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func normalizeDRBenchApps(values []DRBenchApp) ([]DRBenchApp, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("at least one application is required")
	}
	if len(values) > maxAdapterListEntries {
		return nil, fmt.Errorf("application manifest exceeds %d entries", maxAdapterListEntries)
	}
	unique := make(map[DRBenchApp]struct{}, len(values))
	for _, app := range values {
		if _, exists := drBenchAppDetails[app]; !exists {
			return nil, fmt.Errorf("unknown application %q", app)
		}
		unique[app] = struct{}{}
	}
	apps := make([]DRBenchApp, 0, len(unique))
	for app := range unique {
		apps = append(apps, app)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i] < apps[j] })
	return apps, nil
}

func validateDRBenchRecord(record DRBenchRecord) error {
	if record.DocumentCount < 0 || record.InsightCount < 0 || record.ExternalInsightCount < 0 || record.DistractorCount < 0 {
		return fmt.Errorf("benchmark counts must not be negative")
	}
	if record.ExternalInsightCount > record.InsightCount {
		return fmt.Errorf("external insight count exceeds total insight count")
	}
	if len(record.Company.KeyProductsAndServices) > maxAdapterListEntries || len(record.Company.TargetMarkets) > maxAdapterListEntries || len(record.Company.ComplianceCertifications) > maxAdapterListEntries {
		return fmt.Errorf("company profile list exceeds %d entries", maxAdapterListEntries)
	}
	values := []string{
		record.question, record.Date, record.Difficulty, record.Industry, record.Domain,
		record.Company.Name, record.Company.Industry, record.Company.Headquarters,
		record.Company.Size, record.Company.Employees, record.Company.AnnualRevenue,
		record.Company.MarketPosition, record.Company.Description,
		record.Persona.Name, record.Persona.Role, record.Persona.Department,
		record.Persona.Seniority, record.Persona.Email, record.Persona.Responsibilities,
	}
	values = append(values, record.Company.KeyProductsAndServices...)
	values = append(values, record.Company.TargetMarkets...)
	values = append(values, record.Company.ComplianceCertifications...)
	appKeys := make([]DRBenchApp, 0, len(record.AppUsers))
	for app := range record.AppUsers {
		appKeys = append(appKeys, app)
	}
	sort.Slice(appKeys, func(i, j int) bool { return appKeys[i] < appKeys[j] })
	for _, app := range appKeys {
		if _, exists := drBenchAppDetails[app]; !exists {
			return fmt.Errorf("username supplied for unknown application %q", app)
		}
		values = append(values, record.AppUsers[app])
	}
	return validateAdapterText(values...)
}

func drBenchUsers(record DRBenchRecord, apps []DRBenchApp) (map[DRBenchApp]string, error) {
	users := make(map[DRBenchApp]string, len(apps))
	for _, app := range apps {
		user := strings.TrimSpace(record.AppUsers[app])
		if user == "" {
			user = drBenchAppDetails[app].defaultUser
		}
		if !drBenchUsernamePattern.MatchString(user) {
			return nil, fmt.Errorf("application %q username has an unsafe form", app)
		}
		users[app] = user
	}
	return users, nil
}

func drBenchInstruction(record DRBenchRecord, apps []DRBenchApp, users map[DRBenchApp]string) string {
	appLines := make([]string, 0, len(apps))
	for _, app := range apps {
		details := drBenchAppDetails[app]
		guidance := details.guidance
		if strings.Contains(guidance, "%s") {
			guidance = fmt.Sprintf(guidance, users[app])
		}
		appLines = append(appLines, fmt.Sprintf("- **%s** (log in as `%s`, password in `$DRBENCH_%s_PASS`) — %s", app, users[app], strings.ToUpper(string(app)), guidance))
	}
	dateLine := ""
	if date := strings.TrimSpace(record.Date); date != "" {
		dateLine = "Today's date is " + date + "."
	}
	template := `# Deep research request

%s

## Who you are

You are working on behalf of:

%s

## Company context

%s

%s

## Where to research

Your company's systems are running and reachable over the network. Nothing is on this machine's filesystem; query the applications. Each app has its own login, with its password already exported in the environment:

%s

Downloaded documents may be PDF, DOCX, XLSX, PPTX, or JSONL. Convert them with {{bt}}extract-text <path>{{bt}}. Not every document is relevant.

You also have internet access and a {{bt}}web_search{{bt}} tool. Some required public information may exist nowhere in the company's systems, so research the open web as well.

If a service seems unreachable, check {{bt}}http://drbench:8099/health{{bt}}.

## What to deliver

Write a research report to {{bt}}/app/report.md{{bt}} as Markdown.

- Ground every factual claim in a source, cited inline as {{bt}}[1]{{bt}}, {{bt}}[2]{{bt}}, and so on.
- End with a {{bt}}## References{{bt}} section. Name documents by file name and web pages by full URL.
- Cite email as {{bt}}RoundCube-<sender address>-<recipient address>-<Subject>{{bt}}; use the sender's email address and copy the subject exactly.
- Cite chat as {{bt}}MatterMost-<channel>-<team>-<user>{{bt}}.
- Report only what sources support, cover essential findings, and exclude irrelevant material.
`
	instruction := fmt.Sprintf(template, strings.TrimSpace(record.question), drBenchPersonaBrief(record.Persona), drBenchCompanyBrief(record.Company), dateLine, strings.Join(appLines, "\n"))
	return strings.ReplaceAll(instruction, "{{bt}}", string(rune(96)))
}

func drBenchPersonaBrief(persona DRBenchPersona) string {
	lines := []string{"- **Name:** " + defaultValue(persona.Name, "Unknown")}
	appendLabel := func(label, value string) {
		if value = strings.TrimSpace(value); value != "" {
			lines = append(lines, fmt.Sprintf("- **%s:** %s", label, value))
		}
	}
	appendLabel("Role", persona.Role)
	appendLabel("Department", persona.Department)
	appendLabel("Seniority", persona.Seniority)
	appendLabel("Email", persona.Email)
	appendLabel("Responsibilities", persona.Responsibilities)
	return strings.Join(lines, "\n")
}

func drBenchCompanyBrief(company DRBenchCompany) string {
	lines := []string{"- **Company:** " + defaultValue(company.Name, "Unknown")}
	appendLabel := func(label, value string) {
		if value = strings.TrimSpace(value); value != "" {
			lines = append(lines, fmt.Sprintf("- **%s:** %s", label, value))
		}
	}
	appendLabel("Industry", company.Industry)
	appendLabel("Headquarters", company.Headquarters)
	appendLabel("Size", company.Size)
	appendLabel("Employees", company.Employees)
	appendLabel("Annual revenue", company.AnnualRevenue)
	appendLabel("Market position", company.MarketPosition)
	appendLabel("Description", company.Description)
	appendLabel("Key products and services", strings.Join(company.KeyProductsAndServices, "; "))
	appendLabel("Target markets", strings.Join(company.TargetMarkets, "; "))
	appendLabel("Compliance certifications", strings.Join(company.ComplianceCertifications, "; "))
	return strings.Join(lines, "\n")
}

func validateAdapterText(values ...string) error {
	bytes := 0
	for _, value := range values {
		if !utf8.ValidString(value) {
			return fmt.Errorf("text must be valid UTF-8")
		}
		bytes += len(value)
		if bytes > maxAdapterTextBytes {
			return fmt.Errorf("record text exceeds %d bytes", maxAdapterTextBytes)
		}
	}
	return nil
}

func parseContextBenchID(taskID string) (string, int, error) {
	if len(taskID) > 256 {
		return "", 0, fmt.Errorf("context benchmark task ID exceeds 256 bytes")
	}
	match := contextBenchIDPattern.FindStringSubmatch(taskID)
	if match == nil {
		return "", 0, fmt.Errorf("context benchmark task ID must match cb-<suite>-<index>")
	}
	index, err := strconv.Atoi(match[2])
	if err != nil {
		return "", 0, fmt.Errorf("context benchmark task index is out of range")
	}
	return match[1], index, nil
}

func defaultLabel(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "unclassified"
}

func defaultValue(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}
