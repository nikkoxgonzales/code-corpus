// Package codecorpus embeds the agent guide so the binary can print it.
package codecorpus

import _ "embed"

//go:embed AGENTS.md
var Guide string
