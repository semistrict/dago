package lazycue

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// artifactCollector accumulates per-step screenshots for a single test run
// and writes them to disk. It is safe for the sequential ExecuteSteps loop.
type artifactCollector struct {
	dir         string
	mu          sync.Mutex
	screenshots map[int]string // step index -> relative png filename
}

func newArtifactCollector(dir string) (*artifactCollector, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &artifactCollector{dir: dir, screenshots: map[int]string{}}, nil
}

// sink returns a screenshot sink suitable for Browser.SetScreenshotSink.
func (a *artifactCollector) sink() func(int, string, []byte) {
	return func(stepIndex int, action string, png []byte) {
		name := fmt.Sprintf("step-%02d-%s.png", stepIndex, sanitize(action))
		if err := os.WriteFile(filepath.Join(a.dir, name), png, 0o644); err != nil {
			return
		}
		a.mu.Lock()
		a.screenshots[stepIndex] = name
		a.mu.Unlock()
	}
}

// attach assigns captured screenshot paths back onto step results.
func (a *artifactCollector) attach(results []StepResult) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range results {
		if name, ok := a.screenshots[i]; ok {
			results[i].Screenshot = filepath.Join(a.dir, name)
		}
	}
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// WriteReport renders an HTML report for a set of test results, embedding
// per-step screenshots by relative path. The report is written to
// dir/index.html. Screenshot paths in results are made relative to dir.
func WriteReport(dir string, results []*TestResult) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString(reportHeader)

	// Summary stats.
	var pass, fail, cached, generated, healed int
	var totalCost float64
	var totalIn, totalOut int
	for _, r := range results {
		if r.Pass {
			pass++
		} else {
			fail++
		}
		switch r.Mode {
		case RunModeCached:
			cached++
		case RunModeGenerated:
			generated++
		case RunModeHealed:
			healed++
		}
		totalCost += r.EstimatedCost
		totalIn += r.InputTokens
		totalOut += r.OutputTokens
	}

	fmt.Fprintf(&b, `<div class="summary">
  <h1>lazycue report</h1>
  <div class="stats">
    <span class="stat pass">%d passed</span>
    <span class="stat fail">%d failed</span>
    <span class="stat">%d cached</span>
    <span class="stat">%d generated</span>
    <span class="stat">%d healed</span>
    <span class="stat">$%.3f · %s in / %s out tok</span>
  </div>
</div>
`, pass, fail, cached, generated, healed, totalCost, commaInt(totalIn), commaInt(totalOut))

	for _, r := range results {
		statusClass := "pass"
		statusText := "PASS"
		if !r.Pass {
			statusClass = "fail"
			statusText = "FAIL"
		}
		fmt.Fprintf(&b, `<div class="test %s">
  <div class="test-head">
    <span class="badge %s">%s</span>
    <span class="mode">%s v%d</span>
    <span class="time">%s</span>
  </div>
  <div class="desc">%s</div>
`, statusClass, statusClass, statusText, html.EscapeString(string(r.Mode)), r.CacheVersion,
			r.TotalDuration.Round(time.Millisecond), html.EscapeString(r.Description))

		if r.Error != "" {
			fmt.Fprintf(&b, `  <div class="error">%s</div>`+"\n", html.EscapeString(r.Error))
		}

		b.WriteString(`  <div class="steps">` + "\n")
		for _, s := range r.Steps {
			mark := "✓"
			cls := "ok"
			if !s.Pass {
				mark = "✗"
				cls = "bad"
			}
			fmt.Fprintf(&b, `    <div class="step %s">`+"\n", cls)
			fmt.Fprintf(&b, `      <div class="step-info"><span class="mark">%s</span> <span class="sum">%s</span> <span class="dur">%s</span>`,
				mark, html.EscapeString(s.Summary), s.Duration.Round(time.Millisecond))
			if s.Error != "" {
				fmt.Fprintf(&b, ` <span class="err">%s</span>`, html.EscapeString(s.Error))
			}
			b.WriteString("</div>\n")
			if s.Screenshot != "" {
				rel, err := filepath.Rel(dir, s.Screenshot)
				if err != nil {
					rel = s.Screenshot
				}
				fmt.Fprintf(&b, `      <a href="%s" target="_blank"><img loading="lazy" src="%s"></a>`+"\n", rel, rel)
			}
			b.WriteString("    </div>\n")
		}
		b.WriteString("  </div>\n</div>\n")
	}

	b.WriteString(reportFooter)
	return os.WriteFile(filepath.Join(dir, "index.html"), []byte(b.String()), 0o644)
}

func commaInt(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, byte(c))
	}
	return string(out)
}

const reportHeader = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>lazycue report</title>
<style>
  :root { color-scheme: light dark; }
  * { box-sizing: border-box; }
  body { font-family: ui-sans-serif, system-ui, -apple-system, sans-serif; margin: 0; padding: 1rem; background: #0d1117; color: #e6edf3; }
  h1 { font-size: 1.3rem; margin: 0 0 .5rem; }
  .summary { border-bottom: 1px solid #30363d; padding-bottom: .75rem; margin-bottom: 1rem; }
  .stats { display: flex; flex-wrap: wrap; gap: .5rem; }
  .stat { background: #161b22; border: 1px solid #30363d; border-radius: 999px; padding: .15rem .6rem; font-size: .8rem; }
  .stat.pass { color: #3fb950; }
  .stat.fail { color: #f85149; }
  .test { border: 1px solid #30363d; border-radius: 8px; margin-bottom: 1rem; overflow: hidden; }
  .test.fail { border-color: #f85149; }
  .test-head { display: flex; align-items: center; gap: .5rem; padding: .5rem .75rem; background: #161b22; }
  .badge { font-weight: 700; font-size: .75rem; padding: .1rem .5rem; border-radius: 4px; }
  .badge.pass { background: #1a7f37; color: #fff; }
  .badge.fail { background: #b62324; color: #fff; }
  .mode { font-size: .8rem; color: #8b949e; }
  .time { margin-left: auto; font-size: .8rem; color: #8b949e; }
  .desc { padding: .5rem .75rem; font-size: .9rem; color: #c9d1d9; }
  .error { padding: .5rem .75rem; color: #ffa198; font-family: ui-monospace, monospace; font-size: .8rem; white-space: pre-wrap; }
  .steps { display: flex; flex-direction: column; gap: .5rem; padding: .5rem .75rem .75rem; }
  .step { border: 1px solid #21262d; border-radius: 6px; padding: .4rem; }
  .step-info { font-size: .82rem; display: flex; gap: .4rem; align-items: baseline; flex-wrap: wrap; }
  .step .mark { font-weight: 700; }
  .step.ok .mark { color: #3fb950; }
  .step.bad .mark { color: #f85149; }
  .sum { font-family: ui-monospace, monospace; }
  .dur { color: #8b949e; }
  .err { color: #ffa198; font-family: ui-monospace, monospace; }
  .step img { display: block; margin-top: .4rem; max-width: 240px; width: 100%; height: auto; border: 1px solid #30363d; border-radius: 4px; }
</style>
</head>
<body>
`

const reportFooter = `</body>
</html>
`
