//go:build js

package main

// runParseChild on js/wasm is a deliberate refusal: §9.1's table says document parsing is
// "Unavailable" in the extension's copy and that "the extension may not parse a document; it sends
// bytes to the core, which routes them to the parser". A wasm build that quietly parsed in-page
// would be the failure that row exists to prevent.
func runParseChild(args []string) int {
	return fatalf("parse-child: document parsing is unavailable in the js/wasm target (docs/01-collectors.md §9.1); send the document to capture-core")
}
