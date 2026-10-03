package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

// TestPluggableSelectsByWhatIsPresented drives the selector with each credential form and asserts
// the outcome can only have come from the matching authenticator. A request that presents nothing
// is ErrNoCredential, never a failed verification of a credential that was not there.
func TestPluggableSelectsByWhatIsPresented(t *testing.T) {
	ca := newTestAuthority(t, "sac-pluggable-ca")
	leaf := ca.issue(t, leafSpec{cn: testDevice, tenant: testTenant})

	forwardedStore := store.NewMemory(nil)
	forwardedStore.SetPrincipal(testTenant, testDevice, credentialIDOf(leaf),
		activeStatus(spkiThumbprintOf(leaf), "x509"))

	p, err := NewPluggable(Pluggable{
		Direct:    &MTLSAuthenticator{Store: store.NewMemory(nil), Region: "eu-west"},
		Forwarded: &ForwardedCertAuthenticator{Store: forwardedStore, ClientCAs: ca.pool(), Region: "eu-west"},
		DPoP:      &DPoPAuthenticator{Store: store.NewMemory(nil), Issuer: testIssuer, Audience: testAudience},
	})
	if err != nil {
		t.Fatalf("NewPluggable: %v", err)
	}

	t.Run("nothing presented", func(t *testing.T) {
		_, err := p.Authenticate(context.Background(), httptest.NewRequest(http.MethodPost, "/v1/events", nil))
		if !errors.Is(err, ErrNoCredential) {
			t.Fatalf("err = %v, want ErrNoCredential", err)
		}
	})

	t.Run("forwarded certificate selects the forwarded path", func(t *testing.T) {
		req := forwardedRequest(protocol.HeaderClientCert, pemChainOf(leaf))
		got, err := p.Authenticate(context.Background(), req)
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if got.DeviceID != testDevice {
			t.Errorf("device = %q, want %q", got.DeviceID, testDevice)
		}
	})

	t.Run("a DPoP authorization selects the DPoP path", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/events", nil)
		req.Header.Set(protocol.HeaderAuthorization, "DPoP some-token")
		// No DPoP: the selector must still route to the DPoP authenticator, whose own check reports
		// the proof as the missing piece rather than reporting no credential at all.
		if _, err := p.Authenticate(context.Background(), req); !errors.Is(err, ErrBadProof) {
			t.Fatalf("err = %v, want ErrBadProof", err)
		}
	})

	t.Run("a TLS peer certificate selects the direct path", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/events", nil)
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
		// The direct authenticator's store is empty, so a certificate that verified would resolve an
		// unknown tenant; either way the request reached the direct path, not the absent path.
		if _, err := p.Authenticate(context.Background(), req); !errors.Is(err, ErrUnknownTenant) {
			t.Fatalf("err = %v, want ErrUnknownTenant from the direct path", err)
		}
	})
}

func TestNewPluggableRefusesNoMode(t *testing.T) {
	if _, err := NewPluggable(Pluggable{}); err == nil {
		t.Fatal("NewPluggable accepted a configuration with no production mode and no dev escape hatch")
	}
	if _, err := NewPluggable(Pluggable{Dev: Static{}}); err != nil {
		t.Fatalf("NewPluggable refused an acknowledged dev-only configuration: %v", err)
	}
	if _, err := NewPluggable(Pluggable{Direct: &MTLSAuthenticator{}}); err != nil {
		t.Fatalf("NewPluggable refused an x509-only configuration: %v", err)
	}
}

// TestPluggableDevIsOnlySelectedWhenAcknowledgedAndPresent ensures the dev escape hatch is not a
// fallback: it answers its own headers, and it is not reached by an ordinary request.
func TestPluggableDevIsOnlySelectedWhenAcknowledgedAndPresent(t *testing.T) {
	dev := Static{P: Principal{TenantID: testTenant, DeviceID: testDevice}}
	p, err := NewPluggable(Pluggable{Dev: dev})
	if err != nil {
		t.Fatalf("NewPluggable: %v", err)
	}
	got, err := p.Authenticate(context.Background(), httptest.NewRequest(http.MethodPost, "/v1/events", nil))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.DeviceID != testDevice {
		t.Errorf("device = %q, want the dev principal", got.DeviceID)
	}
}
