// Package targets assembles the registry of every target's parser. It is separate from package
// parsers because each target's package depends on that one.
//
// chatgpt.com (ChatGPT Desktop) has no parser of its own yet: requests to it are read by the
// generic parser, and the proxy records a block or warning rule there as logged.
package targets

import (
	"github.com/shadow-ai-capture/device/capture-core/parsers"
	"github.com/shadow-ai-capture/device/capture-core/parsers/anthropic"
	"github.com/shadow-ai-capture/device/capture-core/parsers/claudeai"
	"github.com/shadow-ai-capture/device/capture-core/parsers/gemini"
	"github.com/shadow-ai-capture/device/capture-core/parsers/openai"
)

var registry = parsers.NewRegistry(anthropic.Parser{}, openai.Parser{}, gemini.Parser{}, gemini.Upload{}, gemini.CodeAssist{},
	claudeai.RPC{}, claudeai.Completion{})

// Registry returns the registry the proxies read request bodies through. It holds no state, so
// one is shared.
func Registry() *parsers.Registry { return registry }
