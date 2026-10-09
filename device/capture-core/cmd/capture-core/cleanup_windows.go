//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows"
)

// firewallRuleRemover removes rules through the Windows Firewall's INetFwPolicy2.
func firewallRuleRemover() func(string) (int, error) { return removeFirewallRules }

// uninstallLogPath is %WINDIR%\Temp\ShadowAICapture-uninstall.log.
func uninstallLogPath() string {
	dir, err := windows.GetWindowsDirectory()
	if err != nil || dir == "" {
		dir = os.Getenv("WINDIR")
	}
	return filepath.Join(dir, "Temp", uninstallLogName)
}

const (
	sFalse          = 0x00000001 // S_FALSE: COM was already initialised on this thread
	rpcEChangedMode = 0x80010106 // RPC_E_CHANGED_MODE: initialised in another apartment model
)

// removeFirewallRules removes every rule in the firewall policy whose name starts with prefix.
// INetFwRules::Remove removes a rule by name, and a name it no longer finds is no error.
func removeFirewallRules(prefix string) (int, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err == nil || hresultIs(err, sFalse) {
		defer ole.CoUninitialize()
	} else if !hresultIs(err, rpcEChangedMode) {
		return 0, fmt.Errorf("CoInitializeEx: %w", err)
	}

	unknown, err := oleutil.CreateObject("HNetCfg.FwPolicy2")
	if err != nil {
		return 0, fmt.Errorf("opening the firewall policy: %w", err)
	}
	defer unknown.Release()
	policy, err := unknown.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return 0, fmt.Errorf("opening the firewall policy: %w", err)
	}
	defer policy.Release()
	rulesVar, err := oleutil.GetProperty(policy, "Rules")
	if err != nil {
		return 0, fmt.Errorf("reading the firewall rules: %w", err)
	}
	defer rulesVar.Clear()
	rules := rulesVar.ToIDispatch()

	// Several rules can share a name, and Remove takes one name, so each one is listed and removed.
	var names []string
	err = oleutil.ForEach(rules, func(v *ole.VARIANT) error {
		defer v.Clear()
		rule := v.ToIDispatch()
		if rule == nil {
			return nil
		}
		name, err := oleutil.GetProperty(rule, "Name")
		if err != nil {
			return err
		}
		defer name.Clear()
		if n := name.ToString(); strings.HasPrefix(n, prefix) {
			names = append(names, n)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("listing the firewall rules: %w", err)
	}

	removed := 0
	var errs []error
	for _, n := range names {
		res, err := oleutil.CallMethod(rules, "Remove", n)
		if err != nil {
			errs = append(errs, fmt.Errorf("removing the firewall rule %q: %w", n, err))
			continue
		}
		res.Clear()
		removed++
	}
	return removed, errors.Join(errs...)
}

// hresultIs reports whether err is a COM error with one of codes.
func hresultIs(err error, codes ...uint32) bool {
	var oe *ole.OleError
	if !errors.As(err, &oe) {
		return false
	}
	for _, c := range codes {
		if uint32(oe.Code()) == c {
			return true
		}
	}
	return false
}
