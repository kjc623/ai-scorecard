//go:build !js

package main

import (
	"flag"
	"os"
	"time"

	"github.com/shadow-ai-capture/device/classifier-host/parser"
)

// runParseChild is the parser child's whole process: one framed document in, one framed result
// out, then exit. It is spawned by parser/isolation and is never invoked interactively.
//
// The CLASSIFIER_HOST_TESTHOOK_* variables below exist so the isolation tests can drive a child
// that is hostile in the ways §10 must survive — a child that allocates past its cap, hangs,
// exits non-zero or writes a malformed result. Nothing in production sets them, they are read
// only here in the child, and they can only make the child *less* well behaved, never widen what
// it can reach. They are documented in README.md rather than hidden.
func runParseChild(args []string) int {
	fs := flag.NewFlagSet("parse-child", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return parser.Execute(os.Stdin, os.Stdout, parser.DefaultLimits(), testHookFromEnv())
}

func testHookFromEnv() func() {
	allocMB := envInt("CLASSIFIER_HOST_TESTHOOK_ALLOC_MB")
	sleepMS := envInt("CLASSIFIER_HOST_TESTHOOK_SLEEP_MS")
	exitCode := envInt("CLASSIFIER_HOST_TESTHOOK_EXIT")
	garbage := os.Getenv("CLASSIFIER_HOST_TESTHOOK_GARBAGE") != ""
	if allocMB == 0 && sleepMS == 0 && exitCode == 0 && !garbage {
		return nil
	}
	return func() {
		if garbage {
			os.Stdout.WriteString("this is not a result frame")
			os.Stdout.Close()
			os.Exit(0)
		}
		if exitCode != 0 {
			os.Exit(exitCode)
		}
		if sleepMS > 0 {
			time.Sleep(time.Duration(sleepMS) * time.Millisecond)
		}
		if allocMB > 0 {
			// Touch every page so the allocation becomes commit charge: a job object's
			// memory limit and a residency sample both count touched pages, not reserved
			// address space.
			const chunk = 4 << 20
			var held [][]byte
			for i := 0; i < allocMB/4; i++ {
				b := make([]byte, chunk)
				for j := 0; j < len(b); j += 4096 {
					b[j] = byte(i)
				}
				held = append(held, b)
			}
			_ = held
		}
	}
}

func envInt(name string) int {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	n := 0
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return 0
		}
		n = n*10 + int(v[i]-'0')
		if n > 1<<30 {
			return 1 << 30
		}
	}
	return n
}
