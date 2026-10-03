//go:build !js

package main

import (
	"flag"
	"os"

	"github.com/shadow-ai-capture/device/classifier-host/internal/testhook"
	"github.com/shadow-ai-capture/device/classifier-host/parser"
)

// runParseChild is the parser child's whole process: one framed document in, one framed result
// out, then exit. It is spawned by parser/isolation and is never invoked interactively.
//
// The CLASSIFIER_HOST_TESTHOOK_* hooks (internal/testhook) exist so the isolation tests can drive a
// child that allocates past its cap, hangs, exits or writes a malformed result — the four ways §10
// requires the parent to survive. Nothing in production sets them; see README.md.
func runParseChild(args []string) int {
	fs := flag.NewFlagSet("parse-child", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return parser.Execute(os.Stdin, os.Stdout, parser.DefaultLimits(), testhook.FromEnv())
}
