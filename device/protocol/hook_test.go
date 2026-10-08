package protocol

import (
	"strings"
	"testing"
)

func TestHookRelayIsACollector(t *testing.T) {
	if !CollectorHookRelay.Valid() || CollectorHookRelay != "hook_relay" {
		t.Errorf("collector %q is not the hook_relay code", CollectorHookRelay)
	}
}

func TestHookEvaluateCapsThePrompt(t *testing.T) {
	ev := HookEvaluate{Tool: "claude_code", Event: "UserPromptSubmit", SessionID: "s", PromptText: "hello"}
	ev.CapPrompt()
	if ev.OverCap || ev.PromptBytes != 5 || ev.PromptText != "hello" || ev.Validate() != nil {
		t.Fatalf("a short prompt became %+v (%v)", ev, ev.Validate())
	}
	ev.PromptText = strings.Repeat("a", MaxHookPromptBytes)
	ev.CapPrompt()
	if ev.OverCap || ev.Validate() != nil {
		t.Fatal("a prompt of exactly the cap is not sent whole")
	}
	ev.PromptText = strings.Repeat("a", MaxHookPromptBytes+1)
	ev.CapPrompt()
	if !ev.OverCap || ev.PromptText != "" || ev.PromptBytes != MaxHookPromptBytes+1 || ev.Validate() != nil {
		t.Fatalf("an over-cap prompt is sent as over_cap=%v with %d text bytes and length %d", ev.OverCap, len(ev.PromptText), ev.PromptBytes)
	}
}

func TestHookEvaluateValidate(t *testing.T) {
	ok := HookEvaluate{Tool: "claude_code", Event: "UserPromptSubmit", SessionID: "s", PromptText: "canary-text", PromptBytes: 11}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*HookEvaluate){
		"tool not a key":      func(h *HookEvaluate) { h.Tool = "Claude Code" },
		"no tool":             func(h *HookEvaluate) { h.Tool = "" },
		"no event":            func(h *HookEvaluate) { h.Event = "" },
		"no session":          func(h *HookEvaluate) { h.SessionID = "" },
		"long cwd":            func(h *HookEvaluate) { h.Cwd = strings.Repeat("c", 4097) },
		"wrong length":        func(h *HookEvaluate) { h.PromptBytes = 3 },
		"over the cap":        func(h *HookEvaluate) { h.PromptText = strings.Repeat("a", MaxHookPromptBytes+1) },
		"over cap with text":  func(h *HookEvaluate) { h.OverCap = true },
		"over cap, no length": func(h *HookEvaluate) { h.OverCap, h.PromptText, h.PromptBytes = true, "", 0 },
	} {
		h := ok
		mutate(&h)
		err := h.Validate()
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "canary-text") {
			t.Errorf("%s: the error quotes the prompt: %v", name, err)
		}
	}
}

func TestHookDecisionValidate(t *testing.T) {
	for _, a := range []HookAction{HookAllow, HookWarn, HookBlock} {
		if err := (HookDecision{Action: a, RuleID: "policy.default"}).Validate(); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
	for name, d := range map[string]HookDecision{
		"redact":       {Action: "redact"},
		"empty":        {},
		"long message": {Action: HookWarn, Message: strings.Repeat("m", MaxNotifyBody+1)},
		"long rule":    {Action: HookBlock, RuleID: strings.Repeat("r", 129)},
	} {
		if d.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
