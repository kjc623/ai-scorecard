package main

import (
	"runtime"
	"slices"

	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/toolconfig"
)

// appProcesses lists the running processes of the catalog apps.
func (s *service) appProcesses(apps []string) ([]toolconfig.AppProcess, error) {
	b := s.currentBundle()
	if b == nil {
		return nil, nil
	}
	entries, err := hostinfo.Processes()
	if err != nil {
		return nil, err
	}
	return appProcessesOf(b, catalogPlatform(runtime.GOOS), entries, hostinfo.ProcessInfo, apps), nil
}

// appProcessesOf picks the processes of the catalog apps out of entries, each with the app of the
// process that started it. Only a process whose executable's name the catalog lists is described,
// as describing one verifies its image's signature; a packaged app is found when its executable
// shares a name with one the catalog lists, as the Claude desktop app's does with Claude Code's.
func appProcessesOf(b *policy.Bundle, platform string, entries []hostinfo.ProcessEntry, describe func(pid uint32) (hostinfo.Process, error), apps []string) []toolconfig.AppProcess {
	type known struct {
		p   hostinfo.Process
		app string
	}
	byPID := map[uint32]known{}
	for _, e := range entries {
		if len(b.AppByExe(platform, e.Name)) == 0 {
			continue
		}
		p, err := describe(e.PID)
		if err != nil {
			continue
		}
		if app, ok := b.AppOfProcess(platform, p.Image, p.Package); ok {
			byPID[e.PID] = known{p, app}
		}
	}
	var out []toolconfig.AppProcess
	for _, e := range entries {
		k, ok := byPID[e.PID]
		if !ok || !slices.Contains(apps, k.app) {
			continue
		}
		ap := toolconfig.AppProcess{PID: e.PID, Started: k.p.Started, Session: k.p.Session, App: k.app}
		// A parent that started after the process holds a reused PID.
		if parent, ok := byPID[e.Parent]; ok && e.Parent != e.PID && !parent.p.Started.After(k.p.Started) {
			ap.Parent = parent.app
		}
		out = append(out, ap)
	}
	return out
}
