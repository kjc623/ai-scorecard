//go:build !js

package main

import (
	"github.com/shadow-ai-capture/device/classifier-host/docparse"
	"github.com/shadow-ai-capture/device/classifier-host/parser/isolation"
)

// newParserRunner wires the §10 parser child. The child is this same executable invoked as
// `parse-child`, which is what keeps "one document, one process" and the parent's cap/timeout/kill
// in one binary rather than in a second artefact to ship and sign.
func newParserRunner() docparse.Parser {
	cmd := isolation.DefaultCommand()
	if cmd.Path == "" {
		return nil
	}
	return isolation.New(cmd, isolation.DefaultLimits())
}
