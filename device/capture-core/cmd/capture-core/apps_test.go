package main

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/toolconfig"
)

// The processes of the asked-for apps are listed with the app that started each: the Claude desktop
// app is told from Claude Code by its package although both run claude.exe, a process the catalog
// does not name by its executable is never described, a process that cannot be described is left
// out, and a parent that started after its child is a reused PID.
func TestAppProcessesOf(t *testing.T) {
	b := &policy.Bundle{Catalog: []policy.CatalogApp{
		{AppKey: "claude_code", Signals: []policy.CatalogSignal{{Platform: "windows", Kind: policy.SignalWindowsExe, Value: "claude.exe"}}},
		{AppKey: "claude_desktop", Signals: []policy.CatalogSignal{{Platform: "windows", Kind: policy.SignalWindowsAppx, Value: "Claude_pzs8sxrjxfjjc"}}},
		{AppKey: "codex", Signals: []policy.CatalogSignal{{Platform: "windows", Kind: policy.SignalWindowsExe, Value: "codex.exe"}}},
	}}
	t0 := time.Date(2026, 10, 13, 20, 11, 54, 0, time.UTC)
	const desktopImage = `C:\Program Files\WindowsApps\Claude_2.37337.1.0_x64__pzs8sxrjxfjjc\app\Claude.exe`
	procs := map[uint32]hostinfo.Process{
		15140: {Image: desktopImage, Package: "Claude_pzs8sxrjxfjjc", Started: t0, Session: 1},
		15292: {Image: `C:\Users\k\AppData\Roaming\Claude\claude-code\2.1.295\d2e29494ec83\claude.exe`, Started: t0.Add(53 * time.Second), Session: 1},
		700:   {Image: `C:\Users\k\.local\bin\claude.exe`, Started: t0.Add(time.Hour), Session: 2},
		800:   {Image: `C:\Users\k\.local\bin\claude.exe`, Started: t0.Add(time.Hour), Session: 2},
		900:   {Image: `C:\tools\codex.exe`, Started: t0, Session: 2},
		1000:  {Image: desktopImage, Package: "Claude_pzs8sxrjxfjjc", Started: t0.Add(3 * time.Hour), Session: 2},
	}
	var described []uint32
	describe := func(pid uint32) (hostinfo.Process, error) {
		described = append(described, pid)
		p, ok := procs[pid]
		if !ok {
			return hostinfo.Process{}, errors.New("the process has exited")
		}
		p.PID = pid
		return p, nil
	}
	entries := []hostinfo.ProcessEntry{
		{PID: 15140, Parent: 4000, Name: "Claude.exe"},
		{PID: 15292, Parent: 15140, Name: "claude.exe"},
		{PID: 600, Parent: 1, Name: "pwsh.exe"},
		{PID: 700, Parent: 600, Name: "claude.exe"},
		// Its parent's PID now names a process that started after it.
		{PID: 800, Parent: 1000, Name: "claude.exe"},
		{PID: 900, Parent: 600, Name: "codex.exe"},
		{PID: 950, Parent: 600, Name: "claude.exe"},
		{PID: 1000, Parent: 4000, Name: "Claude.exe"},
	}

	got := appProcessesOf(b, "windows", entries, describe, []string{"claude_code", "claude_desktop"})
	want := []toolconfig.AppProcess{
		{PID: 15140, Started: t0, Session: 1, App: "claude_desktop"},
		{PID: 15292, Started: t0.Add(53 * time.Second), Session: 1, App: "claude_code", Parent: "claude_desktop"},
		{PID: 700, Started: t0.Add(time.Hour), Session: 2, App: "claude_code"},
		{PID: 800, Started: t0.Add(time.Hour), Session: 2, App: "claude_code"},
		{PID: 1000, Started: t0.Add(3 * time.Hour), Session: 2, App: "claude_desktop"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	for _, pid := range described {
		if pid == 600 {
			t.Fatal("a process the catalog does not name by its executable was described")
		}
	}
}
