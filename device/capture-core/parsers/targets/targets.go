// Package targets assembles the registry of every target's parser. It is separate from package
// parsers because each target's package depends on that one.
//
// claude.ai (Claude Desktop) and chatgpt.com (ChatGPT Desktop) have no parser of their own yet:
// their private backends' shapes are known only from captures, so requests to them are read by
// the generic parser.
package targets

import (
	"github.com/shadow-ai-capture/device/capture-core/parsers"
	"github.com/shadow-ai-capture/device/capture-core/parsers/anthropic"
	"github.com/shadow-ai-capture/device/capture-core/parsers/gemini"
	"github.com/shadow-ai-capture/device/capture-core/parsers/openai"
)

var registry = parsers.NewRegistry(anthropic.Parser{}, openai.Parser{}, gemini.Parser{})

// Registry returns the registry the proxies read request bodies through. It holds no state, so
// one is shared.
func Registry() *parsers.Registry { return registry }
