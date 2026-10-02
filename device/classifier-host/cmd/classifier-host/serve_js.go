//go:build js

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"syscall/js"

	"github.com/shadow-ai-capture/device/classifier-host/classify"
	"github.com/shadow-ai-capture/device/protocol"
)

// runServe on js/wasm registers the in-page classifier and stays resident.
//
// The boundary is copied bytes in both directions, because Go's wasm target has no direct
// Chrome-API access (ADR 0016): `shadowAIClassifier.classify(requestJSON)` returns the JSON of
// classify.Verdict, and the request JSON is exactly protocol.ClassifyRequest — the identity-free
// type. There is no other entry point, so the in-page copy cannot be handed anything the native
// host could not be handed.
func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	releaseDir := fs.String("release", "", "signed release directory")
	pubkey := fs.String("pubkey", "", "hex ed25519 release-signing public key")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	r, err := loadRig(*releaseDir, *pubkey)
	if err != nil {
		return fatalf("serve: %v", err)
	}
	api := map[string]any{
		"version": js.FuncOf(func(this js.Value, args []js.Value) any {
			return r.host.Version()
		}),
		"release": js.FuncOf(func(this js.Value, args []js.Value) any {
			return r.host.Health().Release
		}),
		"classify": js.FuncOf(func(this js.Value, args []js.Value) any {
			if len(args) < 1 {
				return degradedJSON(r, "classify: no request was passed")
			}
			var req protocol.ClassifyRequest
			if err := json.Unmarshal([]byte(args[0].String()), &req); err != nil {
				return degradedJSON(r, "classify: request is not a ClassifyRequest: "+err.Error())
			}
			v := r.host.Classify(context.Background(), req)
			b, err := json.Marshal(v)
			if err != nil {
				return degradedJSON(r, "classify: response does not marshal: "+err.Error())
			}
			return string(b)
		}),
	}
	js.Global().Set("shadowAIClassifier", js.ValueOf(api))
	fmt.Fprintln(os.Stderr, "classifier-host: js/wasm classifier registered as shadowAIClassifier")
	select {} // stay resident: the module is loaded once and answers inline decisions
}

func degradedJSON(r *rig, errText string) string {
	resp := protocol.ClassifyResponse{
		ClassifierVersion: r.host.Version(),
		Labels:            []protocol.Label{},
		Confidence:        protocol.ConfidenceDegraded,
		Stages: []protocol.StageResult{{
			Stage: classify.StageRelease, Ran: false, Failed: true,
			Detail: protocol.DetailHostUnreachable, Err: errText,
		}},
	}
	b, _ := json.Marshal(classify.Verdict{Response: resp, Action: protocol.ActionLogged, DecidedLocally: true,
		EnforcementSuppressed: true, SuppressionCause: "degraded"})
	return string(b)
}
