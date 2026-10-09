package otlp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"google.golang.org/grpc/stats"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/protocol"
)

// unattributedUserRef is the user_ref of a sender no person can be named for. The receiver's port
// is shared by every user on the machine, so such a sender is never given the console user.
const unattributedUserRef = "unattributed"

// Sender is the process that sent a request and the person its records are attributed to. It is
// resolved once per connection.
type Sender struct {
	PID uint32
	// Image is the full path of the sending process's executable.
	Image string
	// Publisher is the image's verified signer, empty when unsigned.
	Publisher string
	// Person is the owner of the sending process, or the unattributed user_ref when the process or
	// its owner could not be named. The receiver never hands a normalizer a nil Person.
	Person *core.Person
	// Resolved reports that the process and its owner were named.
	Resolved bool
}

// errNoPerson: the receiver was given no way to name a person, so it does not look senders up.
var errNoPerson = errors.New("no person resolver is configured")

func unattributed(s Sender) Sender {
	s.Person, s.Resolved = &core.Person{UserRef: unattributedUserRef}, false
	return s
}

// processOf names the process at the client end of a loopback TCP connection the receiver accepted.
func processOf(local, remote net.Addr) (hostinfo.Process, error) {
	l, lok := local.(*net.TCPAddr)
	r, rok := remote.(*net.TCPAddr)
	if !lok || !rok {
		return hostinfo.Process{}, fmt.Errorf("a %s connection has no TCP owner", local.Network())
	}
	pid, err := hostinfo.OwnerOfLocalTCP(l.AddrPort(), r.AddrPort())
	if err != nil {
		return hostinfo.Process{}, err
	}
	return hostinfo.ProcessInfo(pid)
}

// connSender is one connection's sender. It is looked up on the connection's first authenticated
// request, while the client is connected, and kept for the connection's later requests: an
// unauthenticated client causes no lookup, and a slow lookup holds up only its own connection.
type connSender struct {
	transport     string
	local, remote net.Addr

	once   sync.Once
	sender Sender
}

type connKey struct{}

// tagConn starts a connection's context with its unresolved sender.
func tagConn(ctx context.Context, transport string, local, remote net.Addr) context.Context {
	return context.WithValue(ctx, connKey{}, &connSender{transport: transport, local: local, remote: remote})
}

// senderOf is the sender of the connection ctx belongs to, resolving it on first use.
func (r *Receiver) senderOf(ctx context.Context) Sender {
	c, ok := ctx.Value(connKey{}).(*connSender)
	if !ok {
		return unattributed(Sender{})
	}
	c.once.Do(func() { c.sender = r.resolve(c) })
	return c.sender
}

// resolve names a connection's sender and logs it: its PID, image base name and user_ref, nothing
// from a request. A lookup the platform does not support, or a receiver given no person resolver,
// is not an error; any other failure counts one error for the connection.
func (r *Receiver) resolve(c *connSender) Sender {
	s, err := r.lookup(c.local, c.remote)
	if err != nil {
		if !errors.Is(err, hostinfo.ErrUnsupported) && !errors.Is(err, errNoPerson) {
			r.counters.Add(protocol.CounterErrors)
		}
		r.cfg.Log.Printf("otlp: new %s connection: pid %d, image %s, user_ref %s (sender not attributed: %v)",
			c.transport, s.PID, imageBase(s.Image), s.Person.UserRef, err)
		return s
	}
	r.cfg.Log.Printf("otlp: new %s connection: pid %d, image %s, user_ref %s",
		c.transport, s.PID, imageBase(s.Image), s.Person.UserRef)
	return s
}

func (r *Receiver) lookup(local, remote net.Addr) (Sender, error) {
	if r.cfg.Person == nil {
		return unattributed(Sender{}), errNoPerson
	}
	p, err := r.owner(local, remote)
	if err != nil {
		return unattributed(Sender{}), err
	}
	s := Sender{PID: p.PID, Image: p.Image, Publisher: p.Publisher}
	if p.User == nil {
		return unattributed(s), fmt.Errorf("the account process %d runs as is unreadable", p.PID)
	}
	who := r.cfg.Person(*p.User)
	s.Person, s.Resolved = &who, true
	return s, nil
}

// imageBase is the executable's file name, from a Windows or a POSIX path.
func imageBase(image string) string {
	if image == "" {
		return "unknown"
	}
	return image[strings.LastIndexAny(image, `\/`)+1:]
}

// connTagger tags each gRPC connection with its unresolved sender.
type connTagger struct{}

func (connTagger) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	return tagConn(ctx, "grpc", info.LocalAddr, info.RemoteAddr)
}

func (connTagger) HandleConn(context.Context, stats.ConnStats) {}

func (connTagger) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context { return ctx }

func (connTagger) HandleRPC(context.Context, stats.RPCStats) {}
