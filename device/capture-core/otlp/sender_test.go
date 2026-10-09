package otlp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/protocol"
)

// testPerson names a sender's owner the way the service's person resolver would, from the account.
func testPerson(u hostinfo.User) core.Person { return core.Person{UserRef: "u_test_" + u.SID} }

// ownerTable stands in for the system's TCP owner table on platforms without one: the client end of
// a loopback connection to one of the receiver's listeners belongs to this test process and its
// user. Once recordDials is called, only connections dialled through dial have a row.
type ownerTable struct {
	r    *Receiver
	exe  string
	user hostinfo.User

	mu      sync.Mutex
	dialled map[string]bool
	calls   int
	err     error
	noUser  bool
}

func newOwnerTable(t *testing.T, r *Receiver) *ownerTable {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return &ownerTable{r: r, exe: exe, user: hostinfo.User{SID: me.Uid, Account: me.Username, Source: "process"}}
}

func (o *ownerTable) ref() string { return testPerson(o.user).UserRef }

func (o *ownerTable) lookup(local, remote net.Addr) (hostinfo.Process, error) {
	o.mu.Lock()
	o.calls++
	err, noUser := o.err, o.noUser
	known := o.dialled == nil || o.dialled[remote.String()]
	o.mu.Unlock()
	if err != nil {
		return hostinfo.Process{}, err
	}
	ra, ok := remote.(*net.TCPAddr)
	if l := local.String(); (l != o.r.HTTPAddr() && l != o.r.GRPCAddr()) || !ok || !ra.IP.IsLoopback() || !known {
		return hostinfo.Process{}, fmt.Errorf("no TCP row has %s as its client end and %s as its server end", remote, local)
	}
	p := hostinfo.Process{PID: uint32(os.Getpid()), Image: o.exe}
	if !noUser {
		u := o.user
		p.User = &u
	}
	return p, nil
}

func (o *ownerTable) recordDials() {
	o.mu.Lock()
	o.dialled = map[string]bool{}
	o.mu.Unlock()
}

// dial connects and records the client end of the connection.
func (o *ownerTable) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	if o.dialled != nil {
		o.dialled[c.LocalAddr().String()] = true
	}
	o.mu.Unlock()
	return c, nil
}

func (o *ownerTable) set(err error, noUser bool) {
	o.mu.Lock()
	o.err, o.noUser = err, noUser
	o.mu.Unlock()
}

func (o *ownerTable) lookups() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

const canary = "CANARY-5d1e-prompt-text"

// clients are an HTTP client and a gRPC client that each keep one connection to the receiver, dialled
// through the owner table.
type clients struct {
	http  *http.Client
	logs  collogspb.LogsServiceClient
	trace coltracepb.TraceServiceClient
	token string
	url   string
}

func dialClients(t *testing.T, f fixture) clients {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{DialContext: f.owner.dial}}
	t.Cleanup(hc.CloseIdleConnections)
	conn, err := grpc.NewClient(f.r.GRPCAddr(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) { return f.owner.dial(ctx, "tcp", addr) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return clients{
		http:  hc,
		logs:  collogspb.NewLogsServiceClient(conn),
		trace: coltracepb.NewTraceServiceClient(conn),
		token: f.r.Token(),
		url:   "http://" + f.r.HTTPAddr(),
	}
}

func (c clients) postLogs(t *testing.T, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.url+"/v1/logs", strings.NewReader(string(logsRequest(canary))))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func (c clients) authorized() context.Context {
	return metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+c.token))
}

func (c clients) exportLogs(t *testing.T) {
	t.Helper()
	req := &collogspb.ExportLogsServiceRequest{}
	if err := proto.Unmarshal(logsRequest(canary), req); err != nil {
		t.Fatal(err)
	}
	if _, err := c.logs.Export(c.authorized(), req); err != nil {
		t.Fatalf("grpc logs export: %v", err)
	}
}

func (c clients) exportSpans(t *testing.T) {
	t.Helper()
	req := &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: tool}}},
			{Key: "user.prompt", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: canary}}},
		}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
			TraceId: []byte("0123456789abcdef"), SpanId: []byte("01234567"), Name: canary,
		}}}},
	}}}
	if _, err := c.trace.Export(c.authorized(), req); err != nil {
		t.Fatalf("grpc trace export: %v", err)
	}
}

func (f fixture) senders() []Sender {
	f.norm.mu.Lock()
	defer f.norm.mu.Unlock()
	return append([]Sender(nil), f.norm.senders...)
}

func (f fixture) connectionLines() []string {
	var out []string
	for _, l := range strings.Split(f.log.all(), "\n") {
		if strings.HasPrefix(l, "otlp: new ") {
			out = append(out, l)
		}
	}
	return out
}

func (f fixture) errorCount() uint64 {
	return f.r.Counters().Cumulative()[protocol.CounterErrors]
}

// A client dialling either listener is attributed to its own process and that process's user, looked
// up once per connection, and each connection is logged once with the PID, image and user_ref only.
func TestSenderIsTheDiallingProcessAndItsOwner(t *testing.T) {
	f := started(t)
	f.owner.recordDials()
	c := dialClients(t, f)

	// An unauthenticated request causes no lookup.
	if code := c.postLogs(t, ""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request: %d", code)
	}
	if n := f.owner.lookups(); n != 0 {
		t.Fatalf("an unauthenticated connection was looked up %d times", n)
	}
	refused := f.errorCount()

	for range 2 {
		if code := c.postLogs(t, c.token); code != http.StatusOK {
			t.Fatalf("http export: %d", code)
		}
	}
	c.exportLogs(t)
	c.exportSpans(t)

	if n := f.owner.lookups(); n != 2 {
		t.Fatalf("%d lookups for one HTTP and one gRPC connection, want 2", n)
	}
	senders := f.senders()
	if len(senders) != 4 {
		t.Fatalf("normalizer got %d senders, want 4", len(senders))
	}
	for _, s := range senders {
		if !s.Resolved || s.PID != uint32(os.Getpid()) || s.Image != f.owner.exe || s.Person == nil || s.Person.UserRef != f.owner.ref() {
			t.Errorf("sender %+v (person %+v), want this process %d and user_ref %s", s, s.Person, os.Getpid(), f.owner.ref())
		}
	}
	if n := f.errorCount() - refused; n != 0 {
		t.Fatalf("errors rose by %d over attributed requests", n)
	}

	lines := f.connectionLines()
	if len(lines) != 2 {
		t.Fatalf("logged %d connection lines, want one per connection: %q", len(lines), f.log.all())
	}
	for _, l := range lines {
		for _, want := range []string{fmt.Sprintf("pid %d", os.Getpid()), "image " + imageBase(f.owner.exe), "user_ref " + f.owner.ref()} {
			if !strings.Contains(l, want) {
				t.Errorf("connection line %q lacks %q", l, want)
			}
		}
	}
	if strings.Contains(f.log.all(), canary) {
		t.Fatalf("the log quotes a request: %q", f.log.all())
	}
}

// A failed lookup leaves the request accepted and its records unattributed, never the console user,
// and counts one error per connection.
func TestFailedLookupIsUnattributed(t *testing.T) {
	for name, fail := range map[string]func(*ownerTable){
		"no process": func(o *ownerTable) { o.set(errors.New("the connection has no owner row"), false) },
		"no account": func(o *ownerTable) { o.set(nil, true) },
	} {
		t.Run(name, func(t *testing.T) {
			f := started(t)
			fail(f.owner)
			c := dialClients(t, f)
			for range 2 {
				if code := c.postLogs(t, c.token); code != http.StatusOK {
					t.Fatalf("http export with a failed lookup: %d", code)
				}
			}
			c.exportLogs(t)

			senders := f.senders()
			if len(senders) != 3 {
				t.Fatalf("normalizer got %d senders, want 3", len(senders))
			}
			for _, s := range senders {
				if s.Resolved || s.Person == nil || s.Person.UserRef != unattributedUserRef {
					t.Errorf("sender %+v (person %+v), want unattributed", s, s.Person)
				}
			}
			if n := f.errorCount(); n != 2 {
				t.Fatalf("errors = %d, want one per connection (2)", n)
			}
			lines := f.connectionLines()
			if len(lines) != 2 || !strings.Contains(lines[0], "user_ref "+unattributedUserRef) {
				t.Fatalf("connection lines %q", lines)
			}
			if strings.Contains(f.log.all(), canary) {
				t.Fatalf("the log quotes a request: %q", f.log.all())
			}
		})
	}
}

// Where the platform has no TCP owner table, every sender is unattributed and nothing is counted.
func TestUnsupportedLookupIsUnattributedWithoutAnError(t *testing.T) {
	f := started(t)
	f.owner.set(hostinfo.ErrUnsupported, false)
	c := dialClients(t, f)
	if code := c.postLogs(t, c.token); code != http.StatusOK {
		t.Fatalf("http export: %d", code)
	}
	c.exportLogs(t)
	for _, s := range f.senders() {
		if s.Resolved || s.Person == nil || s.Person.UserRef != unattributedUserRef {
			t.Errorf("sender %+v, want unattributed", s)
		}
	}
	if n := f.errorCount(); n != 0 {
		t.Fatalf("errors = %d on a platform without the lookup", n)
	}
}

// A receiver given no person resolver attributes every sender to unattributed without a lookup.
func TestNoPersonResolverLooksNothingUp(t *testing.T) {
	norm := &fakeNormalizer{}
	r, err := New(Config{TokenPath: t.TempDir() + "/" + TokenFile, HTTPListen: "127.0.0.1:0", GRPCListen: "127.0.0.1:0", Normalizers: []Normalizer{norm}})
	if err != nil {
		t.Fatal(err)
	}
	var looked atomic.Bool
	r.owner = func(net.Addr, net.Addr) (hostinfo.Process, error) { looked.Store(true); return hostinfo.Process{}, nil }
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())
	if resp := post(t, "http://"+r.HTTPAddr()+"/v1/logs", "application/x-protobuf", r.Token(), logsRequest("hello"), false); resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	norm.mu.Lock()
	defer norm.mu.Unlock()
	if looked.Load() || len(norm.senders) != 1 || norm.senders[0].Resolved || norm.senders[0].Person.UserRef != unattributedUserRef {
		t.Fatalf("looked up %v, senders %+v", looked.Load(), norm.senders)
	}
	if n := r.Counters().Cumulative()[protocol.CounterErrors]; n != 0 {
		t.Fatalf("errors = %d", n)
	}
}
