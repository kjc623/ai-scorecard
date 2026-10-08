package hooks

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/shadow-ai-capture/device/protocol"
)

// Adapter translates between one tool's hook format and the relay's messages. Parse turns the
// JSON the tool writes to the hook's stdin into a hook_evaluate; Render turns the service's
// decision into what the tool reads from the hook's stdout and exit code; Allow is the tool's
// "go ahead" output, which the hook prints whenever it fails open. None of them may write
// anywhere, and an error from Parse must not quote the input.
type Adapter interface {
	Parse(event string, stdin []byte) (protocol.HookEvaluate, error)
	Render(event string, d protocol.HookDecision) (stdout []byte, exitCode int)
	Allow(event string) (stdout []byte, exitCode int)
}

// TestTool is the key of the test adapter. Its input is hook_evaluate's own JSON, naming the tool
// it stands in for, and it prints the decision as JSON.
const TestTool = "test"

// adapters is the registry, keyed by the tool key `capture-core --hook <tool>` names.
var adapters = map[string]Adapter{
	TestTool: testAdapter{},
}

// Lookup returns the adapter for a tool key.
func Lookup(tool string) (Adapter, bool) {
	a, ok := adapters[tool]
	return a, ok
}

// testAdapter drives hook mode in the tests and the benchmark.
type testAdapter struct{}

func (testAdapter) Parse(event string, stdin []byte) (protocol.HookEvaluate, error) {
	var ev protocol.HookEvaluate
	dec := json.NewDecoder(bytes.NewReader(stdin))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ev); err != nil {
		return protocol.HookEvaluate{}, errors.New("test hook: the input is not a hook_evaluate")
	}
	ev.Event = event
	return ev, nil
}

func (testAdapter) Render(_ string, d protocol.HookDecision) ([]byte, int) {
	out, err := json.Marshal(d)
	if err != nil {
		return nil, 0
	}
	return append(out, '\n'), 0
}

func (a testAdapter) Allow(event string) ([]byte, int) {
	return a.Render(event, protocol.HookDecision{Action: protocol.HookAllow})
}
