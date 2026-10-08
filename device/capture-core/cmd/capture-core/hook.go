package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/hooks"
	"github.com/shadow-ai-capture/device/capture-core/localipc"
	"github.com/shadow-ai-capture/device/protocol"
)

// hookArg starts hook mode: `capture-core --hook <tool> <event>`, the command a tool's managed
// configuration runs before it sends a prompt.
const hookArg = "--hook"

const (
	// hookDeadline is the hard ceiling on a whole hook invocation, from process start. Past it the
	// hook prints the tool's allow output.
	hookDeadline = 400 * time.Millisecond
	// maxHookStdin is the most a tool may write to the hook's stdin; more is malformed input.
	maxHookStdin = 1 << 20
)

// processStart is when this process began running Go code: the hook's deadline counts from it.
var processStart = time.Now()

// errHookAnswer covers every way the service's answer can be unusable.
var errHookAnswer = errors.New("the service's answer is not a hook_decision")

// runHook relays one tool hook to the service and prints the tool's rendering of the decision.
// It fails open: on a timeout, a dial failure, a refusal, malformed input or a panic it prints the
// adapter's allow output. It writes nothing to stderr and logs nothing, so no prompt text can
// escape through it. It returns the exit code.
func runHook(start time.Time, args []string, stdin io.Reader, stdout io.Writer, dial func(context.Context) (net.Conn, error)) (code int) {
	defer func() {
		if recover() != nil {
			code = 0
		}
	}()
	if len(args) != 2 {
		return 0
	}
	tool, event := args[0], args[1]
	adapter, ok := hooks.Lookup(tool)
	if !ok {
		return 0
	}
	deadline := start.Add(hookDeadline)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	type rendered struct {
		out  []byte
		code int
	}
	done := make(chan *rendered, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- nil
			}
		}()
		d, err := askService(ctx, adapter, event, stdin, dial)
		if err != nil {
			done <- nil
			return
		}
		out, code := adapter.Render(event, d)
		done <- &rendered{out, code}
	}()

	var r *rendered
	select {
	case r = <-done:
	case <-ctx.Done():
	}
	if r == nil {
		out, code := adapter.Allow(event)
		_, _ = stdout.Write(out)
		return code
	}
	_, _ = stdout.Write(r.out)
	return r.code
}

// askService reads the tool's input, sends it as one hook_evaluate and returns the decision.
func askService(ctx context.Context, adapter hooks.Adapter, event string, stdin io.Reader, dial func(context.Context) (net.Conn, error)) (protocol.HookDecision, error) {
	raw, err := io.ReadAll(io.LimitReader(stdin, maxHookStdin+1))
	if err != nil {
		return protocol.HookDecision{}, err
	}
	if len(raw) > maxHookStdin {
		return protocol.HookDecision{}, errors.New("the hook's input is over its limit")
	}
	ev, err := adapter.Parse(event, raw)
	if err != nil {
		return protocol.HookDecision{}, err
	}
	ev.CapPrompt()
	frame, err := hookFrame(ev)
	if err != nil {
		return protocol.HookDecision{}, err
	}
	if len(frame) > localipc.MaxFrameBytes {
		// The prompt's encoding outgrew the frame: it goes as its length only.
		ev.PromptText, ev.OverCap = "", true
		if frame, err = hookFrame(ev); err != nil {
			return protocol.HookDecision{}, err
		}
	}

	conn, err := dial(ctx)
	if err != nil {
		return protocol.HookDecision{}, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if err := localipc.WriteFrame(conn, frame); err != nil {
		return protocol.HookDecision{}, err
	}
	payload, err := localipc.ReadFrame(conn)
	if err != nil {
		return protocol.HookDecision{}, err
	}
	var msg protocol.NativeMessage
	if json.Unmarshal(payload, &msg) != nil || msg.Type != protocol.TypeHookDecision || msg.Version != protocol.Version {
		return protocol.HookDecision{}, errHookAnswer
	}
	var d protocol.HookDecision
	if json.Unmarshal(msg.Body, &d) != nil || d.Validate() != nil {
		return protocol.HookDecision{}, errHookAnswer
	}
	return d, nil
}

// hookFrame is the hook_evaluate frame for ev, after checking it. HTML characters are left
// unescaped so a prompt's encoding stays close to its length.
func hookFrame(ev protocol.HookEvaluate) ([]byte, error) {
	if err := ev.Validate(); err != nil {
		return nil, err
	}
	body, err := marshalUnescaped(ev)
	if err != nil {
		return nil, err
	}
	return marshalUnescaped(protocol.NativeMessage{Type: protocol.TypeHookEvaluate, Version: protocol.Version, ID: "1", Body: body})
}

func marshalUnescaped(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// hookBenchEndpoint is empty in a release build. The hook benchmark links it
// (-ldflags "-X main.hookBenchEndpoint=<endpoint>") to the endpoint of the in-process service it
// measures, which runs as the benchmark's own account.
var hookBenchEndpoint string

// dialHook connects hook mode to the service's endpoint, checking the service serves it.
func dialHook(ctx context.Context) (net.Conn, error) {
	if hookBenchEndpoint != "" {
		return dialOwnAccount(ctx, hookBenchEndpoint)
	}
	return dialNative(ctx)
}
