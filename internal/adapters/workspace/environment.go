package workspace

import (
	"os"
	"strings"
)

// Environment returns the launcher environment without the variables that
// route Git to another checkout, index, or object store. Everything else —
// including user hooks, credential helpers, and signing configuration — is
// inherited unchanged.
func Environment() []string {
	environment := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
			"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
			"GIT_PREFIX", "GIT_IMPLICIT_WORK_TREE", "GIT_TERMINAL_PROMPT", "GIT_MERGE_AUTOEDIT":
			continue
		}
		environment = append(environment, entry)
	}
	return environment
}

// GitEnvironment is the environment for Git commands Vantry runs on a user's
// behalf: routing variables are dropped and Git never waits for a terminal.
func GitEnvironment() []string {
	return append(Environment(), "GIT_TERMINAL_PROMPT=0", "GIT_MERGE_AUTOEDIT=no")
}
