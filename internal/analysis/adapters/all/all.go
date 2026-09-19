// Package all links every built-in analysis adapter into the binary.
package all

import (
	_ "github.com/agentic-lineage/lineage/internal/analysis/adapters/anthropic"
	_ "github.com/agentic-lineage/lineage/internal/analysis/adapters/openai"
	_ "github.com/agentic-lineage/lineage/internal/analysis/adapters/openrouter"
)
