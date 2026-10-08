//go:build windows

package flowmon

import (
	"context"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/discovery"
	"github.com/shadow-ai-capture/device/capture-core/etwsession"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// realHost is a catalog inference domain the test resolves and connects to on port 443. Nothing is
// sent on the connection.
const realHost = "api.openai.com"

// keptEmitter keeps the records the monitor reports.
type keptEmitter struct {
	mu   sync.Mutex
	recs []discovery.Record
}

func (k *keptEmitter) Emit(_ context.Context, _ *core.CounterSet, r discovery.Record) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.recs = append(k.recs, r)
	return nil
}

func (k *keptEmitter) all() []discovery.Record {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]discovery.Record(nil), k.recs...)
}

// A real session delivers this process's DNS answer for a catalog domain, under its own process
// id, and its TCP connect to one of the answered addresses; the monitor over that session reports
// the connection as the domain's app, attributed to the account the test runs as. It needs an
// elevated process and the network.
func TestRealSessionAttributesThisProcessConnection(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("starting an ETW session needs an elevated (administrator) process; run this test elevated")
	}
	src, err := NetworkEvents()
	if err != nil {
		t.Fatalf("opening the DNS-Client and Kernel-Network session: %v", err)
	}
	// The monitor reads the events the test has checked, in order.
	feed := &forwardSource{events: make(chan etwsession.Event, 1024), done: make(chan struct{})}
	bundle := &policy.Bundle{Catalog: []policy.CatalogApp{{AppKey: "openai_api", Category: "inference_api", Signals: []policy.CatalogSignal{
		{Platform: "any", Kind: policy.SignalInferenceDomain, Value: realHost},
	}}}}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	owner := tu.User.Sid.String()
	kept := &keptEmitter{}
	p := New(Config{
		Emitter: kept,
		Events:  func() (Source, error) { return feed, nil },
		Bundles: func() *policy.Bundle { return bundle },
		Person:  func(u hostinfo.User) core.Person { return core.Person{UserRef: "u_" + u.SID} },
	})
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Stop(context.Background()) }()
	defer src.Close()

	addrs, err := net.DefaultResolver.LookupNetIP(context.Background(), "ip", realHost)
	if err != nil || len(addrs) == 0 {
		t.Fatalf("resolving %s: %v", realHost, err)
	}
	dest := addrs[0].Unmap()
	conn, err := net.DialTimeout("tcp", netip.AddrPortFrom(dest, 443).String(), 10*time.Second)
	if err != nil {
		t.Fatalf("connecting to %s: %v", dest, err)
	}
	_ = conn.Close()

	self := uint32(os.Getpid())
	var sawAnswer, sawConnect bool
	deadline := time.After(15 * time.Second)
	for !sawAnswer || !sawConnect {
		select {
		case ev, ok := <-src.Events():
			if !ok {
				t.Fatal("the session stopped delivering events")
			}
			feed.events <- ev
			switch {
			case strings.EqualFold(ev.Provider, DNSClient.GUID):
				if ev.ID != eventQueryCompleted {
					t.Fatalf("DNS-Client event %d, want only %d", ev.ID, eventQueryCompleted)
				}
				if !strings.EqualFold(strings.TrimSuffix(ev.Properties["QueryName"], "."), realHost) || ev.PID != self {
					continue
				}
				found := false
				for _, a := range queryAddresses(ev.Properties["QueryResults"]) {
					found = found || a == dest
				}
				if !found {
					t.Fatalf("this process's answer for %s, %q, does not hold %s", realHost, ev.Properties["QueryResults"], dest)
				}
				if ev.Time.IsZero() {
					t.Fatal("the DNS answer has no time")
				}
				sawAnswer = true
			case strings.EqualFold(ev.Provider, KernelNetwork.GUID):
				if ev.ID != eventConnectIPv4 && ev.ID != eventConnectIPv6 {
					t.Fatalf("Kernel-Network event %d, want only %d or %d", ev.ID, eventConnectIPv4, eventConnectIPv6)
				}
				pid, ok := uintProperty(ev, "PID")
				daddr, err := netip.ParseAddr(ev.Properties["daddr"])
				if !ok || pid != self || err != nil || daddr.WithZone("").Unmap() != dest {
					continue
				}
				if ev.Properties["dport"] != "443" {
					t.Fatalf("this process's connect names port %q, want 443", ev.Properties["dport"])
				}
				sawConnect = true
			default:
				t.Fatalf("event from provider %s", ev.Provider)
			}
		case <-deadline:
			t.Fatalf("within 15 s the session delivered this process's answer %v and connect %v", sawAnswer, sawConnect)
		}
	}

	deadlineRecord := time.Now().Add(5 * time.Second)
	for len(kept.all()) == 0 && time.Now().Before(deadlineRecord) {
		time.Sleep(20 * time.Millisecond)
	}
	recs := kept.all()
	if len(recs) == 0 {
		t.Fatal("the monitor reported no connection")
	}
	r := recs[0]
	if r.Type != protocol.DiscoveryTypeInferenceConnection || r.AppKey != "openai_api" || r.DestinationHost != realHost ||
		r.Person == nil || r.UserRef != "u_"+owner {
		t.Fatalf("record = %+v", r)
	}
}

// forwardSource hands the monitor the events the test read from the real session.
type forwardSource struct {
	events chan etwsession.Event
	once   sync.Once
	done   chan struct{}
}

func (f *forwardSource) Events() <-chan etwsession.Event { return f.events }
func (f *forwardSource) Close()                          { f.once.Do(func() { close(f.done) }) }
