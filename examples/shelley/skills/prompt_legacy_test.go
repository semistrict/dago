package skills

// ToPromptXML retains the copied standalone prompt-rendering contract for the
// original tests. Production prompting is owned by dago's SkillsMiddleware.
func ToPromptXML(skills []Skill) string {
	return RenderPromptXML(skills)
}
