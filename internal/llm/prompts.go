package llm

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"strings"
)

//go:embed prompts/*.md
var promptFS embed.FS

// Prompt is one versioned prompt file from internal/llm/prompts. Version
// is the file's name and the start of its sha256, such as chat-1a2b3c4d5e6f:
// any edit changes it, so the cache and the call log follow the text.
type Prompt struct {
	Name    string
	Text    string
	Version string
}

// LoadPrompt returns the prompt prompts/<name>.md. A missing file panics:
// the files are built in.
func LoadPrompt(name string) Prompt {
	b, err := promptFS.ReadFile("prompts/" + name + ".md")
	if err != nil {
		panic("llm: no prompt " + name)
	}
	sum := sha256.Sum256(b)
	return Prompt{Name: name, Text: strings.TrimSpace(string(b)), Version: name + "-" + hex.EncodeToString(sum[:6])}
}
