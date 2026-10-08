package integration

import (
	"context"
	"testing"
	"time"

	"github.com/shadow-ai-capture/contracts/envelope"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/discovery"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

// discoveryRecord is a record of one discovery_type, with the fields that type carries, on the
// route of the collector that finds it.
func discoveryRecord(t *testing.T, dt envelope.DiscoveryType, at time.Time) discovery.Record {
	t.Helper()
	r := discovery.Record{
		Type:       protocol.DiscoveryType(dt),
		Route:      protocol.RouteInvScan,
		Version:    "1.0.0",
		UserRef:    testUser,
		OccurredAt: at,
	}
	switch dt {
	case envelope.DiscoveryTypeAppInstalled:
		r.Basis, r.AppKey, r.Publisher, r.UserRef = protocol.DetectionBasisInstalledScan, "claude_desktop", "Anthropic, PBC", discovery.UnattributedUserRef
	case envelope.DiscoveryTypeAppRunning:
		r.Basis, r.Route, r.AppKey, r.Publisher = protocol.DetectionBasisProcessEvent, protocol.RouteProcDetect, "claude_desktop", "Anthropic, PBC"
		r.Person = &core.Person{UserRef: testUser, SubjectName: "ada@contoso.com"}
	case envelope.DiscoveryTypeCLIInstalled:
		r.Basis, r.AppKey = protocol.DetectionBasisPackageScan, "claude_code"
	case envelope.DiscoveryTypeIdeExtension:
		r.Basis, r.AppKey, r.HostApp = protocol.DetectionBasisExtensionScan, "github_copilot", "app:vscode"
	case envelope.DiscoveryTypeLocalModel:
		r.Basis, r.AppKey, r.ModelNames = protocol.DetectionBasisPortListen, "ollama", []string{"llama3.1:8b", "qwen2.5-coder:7b"}
	case envelope.DiscoveryTypeInferenceConnection:
		r.Basis, r.Route, r.AppKey, r.Version, r.DestinationHost = protocol.DetectionBasisFlowMetadata, protocol.RouteNetFlow, "cursor", "", "api.openai.com"
	default:
		t.Fatalf("no record for discovery_type %q", dt)
	}
	return r
}

// Every discovery_type the contract defines, emitted twice through the discovery emitter into the
// real pipeline and spool, leaves exactly one envelope that ingest-api accepts.
func TestEveryEmittedDiscoveryIsAcceptedByTheContract(t *testing.T) {
	schema := deviceSubmissionSchema(t)
	for _, mode := range []protocol.CollectionMode{protocol.ModeM0, protocol.ModeM3} {
		for _, dt := range envelope.AllDiscoveryTypes() {
			t.Run(string(mode)+"/"+string(dt), func(t *testing.T) {
				sp := openSpool(t)
				p := newPipeline(t, sp, &recordingClassifier{}, mode)
				dir, err := state.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				em, err := discovery.New(discovery.Config{
					Pipeline: p,
					Dir:      dir,
					Clock:    time.Now,
					Bundles: func() *policy.Bundle {
						b := bundleWith(mode)
						b.Endpoint.DiscoveryDailyBudget = 200
						return b
					},
					DeviceID: testDevice,
				})
				if err != nil {
					t.Fatal(err)
				}
				collector := core.NewCounterSet(time.Now())
				r := discoveryRecord(t, dt, time.Now())
				for range 2 {
					if err := em.Emit(context.Background(), collector, r); err != nil {
						t.Fatalf("Emit: %v", err)
					}
				}
				if got := collector.Cumulative(); got[protocol.CounterEmitted] != 1 || got[protocol.CounterErrors] != 0 {
					t.Fatalf("collector counters = %v", got)
				}
				entries, err := sp.Peek(10)
				if err != nil || len(entries) != 1 {
					t.Fatalf("spool holds %d entries (%v), want 1", len(entries), err)
				}
				if err := acceptLikeIngest(t, schema, entries[0].Payload); err != nil {
					t.Fatalf("ingest would refuse the envelope: %v\n%s", err, entries[0].Payload)
				}
				sub, err := envelope.DecodeDeviceSubmission(entries[0].Payload)
				if err != nil {
					t.Fatal(err)
				}
				d, ok := sub.(*envelope.DeviceDiscovery)
				if !ok {
					t.Fatalf("decoded as %T", sub)
				}
				wantUser := r.UserRef
				if r.Person != nil {
					wantUser = r.Person.UserRef
				}
				if d.DiscoveryType != dt || string(d.Source) != string(r.Route) || d.ToolFingerprint != "app:"+r.AppKey ||
					d.UserRef != wantUser || d.DedupKey != "sha256:"+discovery.Key(testDevice, wantUser, r) ||
					string(d.CollectionMode) != string(mode) {
					t.Fatalf("decoded discovery = %+v", d)
				}
			})
		}
	}
}
