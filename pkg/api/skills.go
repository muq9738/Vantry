package api

// SkillInfo is a Pi-discovered skill the desktop can offer for insertion.
// The Core host reports only metadata Pi itself loaded for the target
// workspace; Vantry never scans skill directories or guesses a list.
type SkillInfo struct {
	// Name is the plain skill name, e.g. "docx".
	Name string `json:"name"`
	// Description is the routing description from the skill's frontmatter.
	Description string `json:"description"`
	// FilePath is the SKILL.md (or equivalent resource) path on the Core host.
	FilePath string `json:"filePath"`
	// Source is Pi's discovery scope ("user", "project", ...) or loader source
	// when no scope is reported.
	Source string `json:"source"`
	// Command is the RPC command name Pi expands from a prompt: "skill:<name>".
	// Inserting "/" + Command into the composer produces what a worker expands.
	Command string `json:"command"`
}
