package daskill

// Builtins returns fresh copies of the standard local skills. They contain no
// ambient service credentials or network behavior and are the lowest-precedence
// source when merged with filesystem skills.
func Builtins() []Skill {
	return []Skill{
		{
			Name: "deepagents-thread-inspector", Description: "Inspect local agent threads read-only and produce bounded summaries or JSON transcripts.",
			Body: "Use the local thread/session inspection command in read-only mode. Ask which thread when it is ambiguous. Prefer a concise summary unless a transcript or latest turn was requested. Bound output, preserve message roles and tool-call relationships, redact likely credentials, and never modify the checkpoint database. If the local database cannot be opened safely, explain that instead of copying or repairing it.",
		},
		{
			Name: "remember", Description: "Capture durable user-approved learnings in project instructions or a reusable skill.",
			Body: "Capture only a learning the user explicitly asked to remember. Put general project guidance in the applicable AGENTS.md and repeatable procedures in a narrowly named SKILL.md. Inspect existing guidance first, preserve its style and scope, avoid secrets and transient facts, make the smallest edit, and report the exact destination. If scope is ambiguous, ask before writing.",
		},
		{
			Name: "skill-creator", Description: "Create or improve a focused Agent Skill with valid metadata and testable instructions.",
			Body: "Create a focused skill for the requested workflow. Inspect related skills, choose a lowercase hyphenated name, write valid SKILL.md frontmatter with a concrete trigger description, keep instructions concise and imperative, and add only the scripts or references needed. Use the skills create command for the skeleton, validate discovery and invocation, and do not include credentials or machine-specific paths.",
		},
	}
}
