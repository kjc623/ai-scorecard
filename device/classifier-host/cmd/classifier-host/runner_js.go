//go:build js

package main

import "github.com/shadow-ai-capture/device/classifier-host/docparse"

// newParserRunner on js/wasm returns nil: there is no process to spawn, and §9.1's table says
// document parsing is unavailable in this target. The host turns that into a degraded verdict
// with `parser_failed`, which is the honest answer — not an empty label set that would look like
// "no sensitive content".
func newParserRunner() docparse.Parser { return nil }
