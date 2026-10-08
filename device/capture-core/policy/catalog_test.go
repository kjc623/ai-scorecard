package policy

import (
	"reflect"
	"strings"
	"testing"
)

// catalogSection is a catalog in control-api's spelling: apps sorted by key, signals by platform,
// kind and value.
const catalogSection = `[
  {"app_key": "anthropic_api", "category": "inference_api", "signals": [
    {"platform": "any", "kind": "inference_domain", "value": "api.anthropic.com"}]},
  {"app_key": "azure_ai_foundry", "category": "inference_api", "signals": [
    {"platform": "any", "kind": "inference_domain", "value": ".openai.azure.com"}]},
  {"app_key": "claude_code", "category": "coding_agent", "signals": [
    {"platform": "any", "kind": "cli_binary", "value": "claude"},
    {"platform": "any", "kind": "inference_domain", "value": "api.anthropic.com"},
    {"platform": "any", "kind": "npm_package", "value": "@anthropic-ai/claude-code"},
    {"platform": "windows", "kind": "windows_exe", "value": "claude.exe"}]},
  {"app_key": "claude_code_vscode", "category": "ide_assistant", "signals": [
    {"platform": "any", "kind": "ide_extension_id", "value": "anthropic.claude-code"}]},
  {"app_key": "lm_studio", "category": "local_runtime", "signals": [
    {"platform": "any", "kind": "listen_port", "value": "1234"},
    {"platform": "any", "kind": "model_store", "value": "~/.lmstudio/models"}]},
  {"app_key": "ollama", "category": "local_runtime", "signals": [
    {"platform": "any", "kind": "listen_port", "value": "11434"},
    {"platform": "windows", "kind": "model_store", "value": "%USERPROFILE%\\.ollama\\models"},
    {"platform": "windows", "kind": "windows_exe", "value": "ollama app.exe"},
    {"platform": "windows", "kind": "windows_exe", "value": "ollama.exe"}]},
  {"app_key": "vscode", "category": "ide", "signals": [
    {"platform": "macos", "kind": "macos_bundle_id", "value": "com.microsoft.VSCode"},
    {"platform": "windows", "kind": "publisher", "value": "Microsoft Corporation"},
    {"platform": "windows", "kind": "windows_exe", "value": "Code.exe"}]},
  {"app_key": "windsurf", "category": "ide", "signals": []}
]`

func TestCatalogDecodes(t *testing.T) {
	v, priv := newKeyPair(t, "policy-key-1")
	b, err := v.Open(signWithMembers(t, priv, "70", map[string]string{"catalog": catalogSection}), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(b.Catalog) != 8 {
		t.Fatalf("catalog has %d apps, want 8", len(b.Catalog))
	}
	want := CatalogApp{AppKey: "vscode", Category: "ide", Signals: []CatalogSignal{
		{Platform: "macos", Kind: SignalMacOSBundleID, Value: "com.microsoft.VSCode"},
		{Platform: "windows", Kind: SignalPublisher, Value: "Microsoft Corporation"},
		{Platform: "windows", Kind: SignalWindowsExe, Value: "Code.exe"},
	}}
	if !reflect.DeepEqual(b.Catalog[6], want) {
		t.Fatalf("catalog[6] = %+v, want %+v", b.Catalog[6], want)
	}

	// A misspelt name inside an app or a signal is an unknown field, so the whole bundle is refused.
	for _, misspelt := range []string{
		strings.Replace(catalogSection, `"app_key": "windsurf"`, `"key": "windsurf"`, 1),
		strings.Replace(catalogSection, `"kind": "cli_binary"`, `"type": "cli_binary"`, 1),
	} {
		if _, err := v.Open(signWithMembers(t, priv, "71", map[string]string{"catalog": misspelt}), nil); CauseOf(err) != CauseSchemaInvalid {
			t.Fatalf("misspelt catalog field cause = %q, want %q (err=%v)", CauseOf(err), CauseSchemaInvalid, err)
		}
	}

	// A bundle without a catalog matches nothing.
	none := testBundle("72")
	if err := none.Validate(); err != nil || none.Catalog != nil || none.AppByCLI("claude") != nil || none.Category("claude_code") != "" {
		t.Fatalf("a bundle without a catalog = %+v, %v", none.Catalog, err)
	}
}

func TestCatalogValidation(t *testing.T) {
	v, priv := newKeyPair(t, "policy-key-1")
	refused := map[string][2]string{
		"unknown category": {`"category": "local_runtime", "signals": [
    {"platform": "any", "kind": "listen_port", "value": "1234"}`, `"category": "local_model", "signals": [
    {"platform": "any", "kind": "listen_port", "value": "1234"}`},
		"empty category":    {`"app_key": "windsurf", "category": "ide"`, `"app_key": "windsurf", "category": ""`},
		"unknown platform":  {`{"platform": "macos", "kind": "macos_bundle_id"`, `{"platform": "darwin", "kind": "macos_bundle_id"`},
		"empty platform":    {`{"platform": "any", "kind": "cli_binary"`, `{"platform": "", "kind": "cli_binary"`},
		"unknown kind":      {`"kind": "cli_binary"`, `"kind": "registry_key"`},
		"empty value":       {`"value": "claude"}`, `"value": ""}`},
		"blank value":       {`"value": "Code.exe"}`, `"value": "  "}`},
		"duplicate app_key": {`"app_key": "windsurf"`, `"app_key": "vscode"`},
	}
	for name, r := range refused {
		section := strings.Replace(catalogSection, r[0], r[1], 1)
		if section == catalogSection {
			t.Fatalf("%s: the replacement did not apply", name)
		}
		if _, err := v.Open(signWithMembers(t, priv, "80", map[string]string{"catalog": section}), nil); CauseOf(err) != CauseSchemaInvalid {
			t.Errorf("%s: cause = %q, want %q (err=%v)", name, CauseOf(err), CauseSchemaInvalid, err)
		}
	}

	accepted := map[string][2]string{
		"every category": {`"app_key": "windsurf", "category": "ide"`, `"app_key": "windsurf", "category": "ai_feature"`},
		"every platform": {`{"platform": "macos", "kind": "macos_bundle_id"`, `{"platform": "linux", "kind": "macos_bundle_id"`},
		"every kind": {`"signals": []`, `"signals": [` + func() string {
			var s []string
			for _, k := range CatalogKinds {
				s = append(s, `{"platform": "any", "kind": "`+k+`", "value": "x"}`)
			}
			return strings.Join(s, ",")
		}() + `]`},
		"no signals": {`"signals": [
    {"platform": "any", "kind": "ide_extension_id", "value": "anthropic.claude-code"}]`, `"signals": []`},
	}
	for name, r := range accepted {
		section := strings.Replace(catalogSection, r[0], r[1], 1)
		if section == catalogSection {
			t.Fatalf("%s: the replacement did not apply", name)
		}
		if _, err := v.Open(signWithMembers(t, priv, "90", map[string]string{"catalog": section}), nil); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
	if _, err := v.Open(signWithMembers(t, priv, "91", map[string]string{"catalog": `[]`}), nil); err != nil {
		t.Errorf("empty catalog: refused: %v", err)
	}
}

func TestCatalogLookups(t *testing.T) {
	v, priv := newKeyPair(t, "policy-key-1")
	b, err := v.Open(signWithMembers(t, priv, "100", map[string]string{"catalog": catalogSection}), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for name, tc := range map[string]struct {
		got, want []string
	}{
		"exe":                            {b.AppByExe("windows", "Code.exe"), []string{"vscode"}},
		"exe in another case":            {b.AppByExe("windows", "CODE.EXE"), []string{"vscode"}},
		"exe with a space":               {b.AppByExe("windows", "ollama app.exe"), []string{"ollama"}},
		"exe on another platform":        {b.AppByExe("macos", "Code.exe"), nil},
		"unknown exe":                    {b.AppByExe("windows", "notepad.exe"), nil},
		"exe is not a cli binary":        {b.AppByExe("windows", "claude"), nil},
		"publisher":                      {b.AppByPublisher("windows", "Microsoft Corporation"), []string{"vscode"}},
		"publisher in another case":      {b.AppByPublisher("windows", "microsoft corporation"), []string{"vscode"}},
		"publisher on macOS":             {b.AppByPublisher("macos", "Microsoft Corporation"), nil},
		"publisher by part":              {b.AppByPublisher("windows", "Microsoft"), nil},
		"exact domain":                   {b.AppByDomain("api.anthropic.com"), []string{"anthropic_api", "claude_code"}},
		"domain in another case":         {b.AppByDomain("API.Anthropic.com."), []string{"anthropic_api", "claude_code"}},
		"subdomain of an exact host":     {b.AppByDomain("x.api.anthropic.com"), nil},
		"suffix domain":                  {b.AppByDomain("contoso.openai.azure.com"), []string{"azure_ai_foundry"}},
		"suffix domain itself":           {b.AppByDomain("openai.azure.com"), []string{"azure_ai_foundry"}},
		"lookalike of a suffix":          {b.AppByDomain("contosoopenai.azure.com"), nil},
		"empty domain":                   {b.AppByDomain(""), nil},
		"extension id":                   {b.AppByExtensionID("anthropic.claude-code"), []string{"claude_code_vscode"}},
		"extension id in mixed case":     {b.AppByExtensionID("Anthropic.Claude-Code"), []string{"claude_code_vscode"}},
		"unknown extension id":           {b.AppByExtensionID("anthropic.claude"), nil},
		"cli binary":                     {b.AppByCLI("claude"), []string{"claude_code"}},
		"unknown cli binary":             {b.AppByCLI("claude2"), nil},
		"npm package":                    {b.AppByNPM("@anthropic-ai/claude-code"), []string{"claude_code"}},
		"npm package name without scope": {b.AppByNPM("claude-code"), nil},
		"port":                           {b.AppsByPort(11434), []string{"ollama"}},
		"other port":                     {b.AppsByPort(1234), []string{"lm_studio"}},
		"unknown port":                   {b.AppsByPort(8080), nil},
	} {
		if !reflect.DeepEqual(tc.got, tc.want) {
			t.Errorf("%s: got %v, want %v", name, tc.got, tc.want)
		}
	}
	for app, want := range map[string]string{"claude_code": "coding_agent", "vscode": "ide", "ollama": "local_runtime", "cursor": "", "": ""} {
		if got := b.Category(app); got != want {
			t.Errorf("Category(%q) = %q, want %q", app, got, want)
		}
	}

	var nilBundle *Bundle
	if nilBundle.AppByExe("windows", "Code.exe") != nil || nilBundle.AppsByPort(11434) != nil || nilBundle.Category("vscode") != "" {
		t.Fatal("a nil bundle matched an app")
	}
}
