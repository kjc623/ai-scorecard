package store_test

import (
	"context"
	"reflect"
	"slices"
	"sort"
	"testing"

	"github.com/shadow-ai-capture/control-api/internal/pgtest"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// TestAppCatalogAgainstPostgres reads the seeded catalog as sac_control: the policy read carries
// every app of ref.app with its signals, apps by key and signals by platform, kind and value, and
// the hosts the device may decrypt; the Settings read names the categories those apps fall in.
func TestAppCatalogAgainstPostgres(t *testing.T) {
	owner := pgtest.Open(t)
	st := store.NewSQL(pgtest.OpenAs(t, "sac_control"))
	ctx := context.Background()
	tenant := pgtest.Tenant(t, owner, "eastus")

	var apps, signals int
	if err := owner.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM ref.app), (SELECT count(*) FROM ref.app_signal)`).Scan(&apps, &signals); err != nil {
		t.Fatal(err)
	}
	in, err := st.PolicyInputs(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Catalog) != apps || apps != 36 {
		t.Fatalf("the policy read has %d apps, ref.app %d; want the 36 seed apps", len(in.Catalog), apps)
	}
	n := 0
	for i, a := range in.Catalog {
		if i > 0 && in.Catalog[i-1].AppKey >= a.AppKey {
			t.Fatalf("apps out of order at %s", a.AppKey)
		}
		if a.Category == "" || len(a.Signals) == 0 {
			t.Fatalf("app %+v has no category or no signals", a)
		}
		ordered := sort.SliceIsSorted(a.Signals, func(x, y int) bool {
			p, q := a.Signals[x], a.Signals[y]
			if p.Platform != q.Platform {
				return p.Platform < q.Platform
			}
			if p.Kind != q.Kind {
				return p.Kind < q.Kind
			}
			return p.Value < q.Value
		})
		if !ordered {
			t.Fatalf("signals of %s out of order: %+v", a.AppKey, a.Signals)
		}
		n += len(a.Signals)
	}
	if n != signals {
		t.Fatalf("the policy read has %d signals, ref.app_signal %d", n, signals)
	}
	for _, a := range in.Catalog {
		if a.AppKey == "vscode" {
			want := []store.CatalogSignal{
				{Platform: "macos", Kind: "macos_bundle_id", Value: "com.microsoft.VSCode"},
				{Platform: "windows", Kind: "publisher", Value: "Microsoft Corporation"},
				{Platform: "windows", Kind: "windows_exe", Value: "Code.exe"},
			}
			if a.Category != "ide" || !reflect.DeepEqual(a.Signals, want) {
				t.Fatalf("vscode = %+v", a)
			}
		}
	}

	// The device decrypts the tls rows' hosts and the inference APIs' domains, and no other app's.
	if !sort.StringsAreSorted(in.InterceptionHosts) {
		t.Fatalf("interception hosts out of order: %v", in.InterceptionHosts)
	}
	for _, h := range []string{"api.githubcopilot.com", "cloudcode-pa.googleapis.com", "api.cohere.com", ".openai.azure.com", ".bedrock-runtime.us-east-1.amazonaws.com"} {
		if !slices.Contains(in.InterceptionHosts, h) {
			t.Errorf("interception hosts lack %s: %v", h, in.InterceptionHosts)
		}
	}
	for _, h := range []string{"claude.ai", ".claude.ai", ".chatgpt.com", ".perplexity.ai"} {
		if slices.Contains(in.InterceptionHosts, h) {
			t.Errorf("interception hosts hold %s, a web app's domain", h)
		}
	}

	s, err := st.Settings(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"chat_assistant", "coding_agent", "ide", "ide_assistant", "inference_api", "local_runtime"}; !reflect.DeepEqual(s.AppCategories, want) {
		t.Fatalf("app categories = %v, want %v", s.AppCategories, want)
	}
}
