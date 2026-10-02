// .integration/repro/goconsume - the independent consumer of the extension's native frame.
//
// It decodes a frame produced by .integration/repro/native-content-seam.mjs (which uses the
// extension's own src/messages.js) into device/protocol's ObservationMessage, i.e. the type the
// seam is declared with, and reports what the Go side actually receives.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/shadow-ai-capture/device/protocol"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: goconsume <frame.json>")
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Println("read:", err)
		os.Exit(2)
	}
	var frame protocol.NativeMessage
	if err := json.Unmarshal(raw, &frame); err != nil {
		fmt.Printf("%s: frame unmarshal FAILED: %v\n", os.Args[1], err)
		return
	}
	var obs protocol.ObservationMessage
	if err := json.Unmarshal(frame.Body, &obs); err != nil {
		fmt.Printf("%s: body unmarshal FAILED: %v\n", os.Args[1], err)
		return
	}
	if err := obs.Validate(); err != nil {
		fmt.Printf("%s: decoded, but Validate refused it: %v\n", os.Args[1], err)
		return
	}
	fmt.Printf("%s: decoded OK content_bytes=%d content_text=%q content_is_binary=%v\n",
		os.Args[1], len(obs.Content), string(obs.Content), obs.ContentIsBinary)
}
