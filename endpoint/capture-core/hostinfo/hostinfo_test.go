package hostinfo

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"math/big"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// fakeRegistry is HKLM as a map: "path" lists subkeys, "path|name" holds a value.
type fakeRegistry struct {
	keys map[string][]string
	strs map[string]string
	ints map[string]uint64
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{keys: map[string][]string{}, strs: map[string]string{}, ints: map[string]uint64{}}
}

func (r *fakeRegistry) addKey(path string) {
	parent, child := path[:strings.LastIndex(path, `\`)], path[strings.LastIndex(path, `\`)+1:]
	r.keys[parent] = append(r.keys[parent], child)
	sort.Strings(r.keys[parent])
}

func (r *fakeRegistry) SubKeys(path string) ([]string, error) {
	if k, ok := r.keys[path]; ok {
		return k, nil
	}
	return nil, ErrNotFound
}

func (r *fakeRegistry) String(path, name string) (string, error) {
	if v, ok := r.strs[path+"|"+name]; ok {
		return v, nil
	}
	return "", ErrNotFound
}

func (r *fakeRegistry) Integer(path, name string) (uint64, error) {
	if v, ok := r.ints[path+"|"+name]; ok {
		return v, nil
	}
	return 0, ErrNotFound
}

// enrol adds one MDM enrolment the way Windows records it.
func (r *fakeRegistry) enrol(guid, provider, entDMID string, state uint64) {
	base := enrollmentsKey + `\` + guid
	r.addKey(base)
	r.strs[base+"|ProviderID"] = provider
	r.ints[base+"|EnrollmentState"] = state
	if entDMID != "" {
		r.strs[base+`\DMClient\`+provider+"|EntDMID"] = entDMID
	}
}

func TestIntuneDeviceIDPicksTheMSDMServerEnrolment(t *testing.T) {
	reg := newFakeRegistry()
	// The noise a real machine carries: enrolments of other providers, and keys that are not
	// enrolments at all (this host has "Context", "Status" and "ValidNodePaths").
	reg.enrol("8345CBE6-CEFC-462A-8219-78F3FC0377C1", "Deploy Authority", "", 1)
	reg.enrol("C429BE2D-071B-4E13-B616-4141C334ECDF", "Cloud Authority", "", 1)
	reg.addKey(enrollmentsKey + `\Context`)
	reg.enrol("0A1B2C3D-0000-0000-0000-000000000001", intuneProvider, "9b1c0f2e-3c44-4b5e-9a61-2d7f0e8c1a55", 1)

	id, note := IntuneDeviceID(reg)
	if id != "9b1c0f2e-3c44-4b5e-9a61-2d7f0e8c1a55" || note != "" {
		t.Fatalf("IntuneDeviceID = %q (%s), want the MS DM Server EntDMID", id, note)
	}
}

func TestIntuneDeviceIDPrefersTheActiveEnrolment(t *testing.T) {
	reg := newFakeRegistry()
	reg.enrol("0A1B2C3D-0000-0000-0000-000000000001", intuneProvider, "11111111-aaaa-4aaa-8aaa-111111111111", 0) // a lingering unenrolment
	reg.enrol("0A1B2C3D-0000-0000-0000-000000000002", intuneProvider, "22222222-bbbb-4bbb-8bbb-222222222222", 1)
	if id, _ := IntuneDeviceID(reg); id != "22222222-bbbb-4bbb-8bbb-222222222222" {
		t.Fatalf("IntuneDeviceID = %q, want the active enrolment's id", id)
	}
}

func TestIntuneDeviceIDRefusesAmbiguity(t *testing.T) {
	reg := newFakeRegistry()
	reg.enrol("0A1B2C3D-0000-0000-0000-000000000001", intuneProvider, "11111111-aaaa-4aaa-8aaa-111111111111", 1)
	reg.enrol("0A1B2C3D-0000-0000-0000-000000000002", intuneProvider, "22222222-bbbb-4bbb-8bbb-222222222222", 1)
	id, note := IntuneDeviceID(reg)
	if id != "" || note == "" {
		t.Fatalf("IntuneDeviceID = %q (%q); two active ids must send none and say why", id, note)
	}
}

func TestIntuneDeviceIDAbsentWithoutEnrolments(t *testing.T) {
	if id, note := IntuneDeviceID(newFakeRegistry()); id != "" || note != "" {
		t.Fatalf("IntuneDeviceID on an empty registry = %q (%q), want nothing", id, note)
	}
}

// testCert issues a certificate with the given subject CN from an issuer with the given CN.
func testCert(t *testing.T, issuerCN, subjectCN string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: issuerCN},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: subjectCN},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func thumbprint(c *x509.Certificate) string {
	sum := sha1.Sum(c.Raw)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

func TestEntraDeviceIDFollowsTheJoinStateToItsCertificate(t *testing.T) {
	device := testCert(t, deviceCertIssuer, "5C2A3B1E-77D0-4E1F-9B6A-0C1D2E3F4A5B")
	other := testCert(t, "Some Other CA", "6D3B4C2F-88E1-4F20-AC7B-1D2E3F4A5B6C")
	p2p := testCert(t, "MS-Organization-P2P-Access [2026]", "7E4C5D30-99F2-4031-BD8C-2E3F4A5B6C7D")
	reg := newFakeRegistry()
	reg.addKey(joinInfoKey + `\` + thumbprint(device))

	id, note := EntraDeviceID(reg, []*x509.Certificate{other, p2p, device})
	if id != "5c2a3b1e-77d0-4e1f-9b6a-0c1d2e3f4a5b" || note != "" {
		t.Fatalf("EntraDeviceID = %q (%s), want the join certificate's CN, lower-cased", id, note)
	}
}

func TestEntraDeviceIDWithoutJoinStateUsesASingleDeviceCertificate(t *testing.T) {
	device := testCert(t, deviceCertIssuer, "5c2a3b1e-77d0-4e1f-9b6a-0c1d2e3f4a5b")
	if id, _ := EntraDeviceID(newFakeRegistry(), []*x509.Certificate{device}); id != "5c2a3b1e-77d0-4e1f-9b6a-0c1d2e3f4a5b" {
		t.Fatalf("EntraDeviceID = %q, want the one device certificate's id", id)
	}
	second := testCert(t, deviceCertIssuer, "6d3b4c2f-88e1-4f20-ac7b-1d2e3f4a5b6c")
	if id, note := EntraDeviceID(newFakeRegistry(), []*x509.Certificate{device, second}); id != "" || note == "" {
		t.Fatalf("EntraDeviceID with two unjoined device certificates = %q (%q), want none and a note", id, note)
	}
}

func TestEntraDeviceIDJoinedButCertificateMissing(t *testing.T) {
	reg := newFakeRegistry()
	reg.addKey(joinInfoKey + `\0123456789ABCDEF0123456789ABCDEF01234567`)
	if id, note := EntraDeviceID(reg, nil); id != "" || note == "" {
		t.Fatalf("EntraDeviceID = %q (%q), want none and a note", id, note)
	}
}

func TestEntraDeviceIDIgnoresANonGUIDSubject(t *testing.T) {
	notADevice := testCert(t, deviceCertIssuer, "not-a-device-id")
	if id, _ := EntraDeviceID(nil, []*x509.Certificate{notADevice}); id != "" {
		t.Fatalf("EntraDeviceID = %q from a non-GUID subject, want none", id)
	}
}

// smbiosStructure assembles one structure: header, formatted area, string set.
func smbiosStructure(typ byte, formatted []byte, strs ...string) []byte {
	out := []byte{typ, byte(4 + len(formatted)), 0x00, 0x00}
	out = append(out, formatted...)
	if len(strs) == 0 {
		return append(out, 0, 0)
	}
	for _, s := range strs {
		out = append(append(out, s...), 0)
	}
	return append(out, 0)
}

// typeOne is a System Information structure whose UUID bytes are the Windows (2.6+) encoding of
// A2219E09-2C68-6D1D-A831-345A6060843C, the UUID WMI reports for the host this was written on.
func typeOne(serialIndex byte) []byte {
	formatted := []byte{1, 2, 3, serialIndex} // manufacturer, product, version, serial
	formatted = append(formatted, 0x09, 0x9e, 0x21, 0xa2, 0x68, 0x2c, 0x1d, 0x6d, 0xa8, 0x31, 0x34, 0x5a, 0x60, 0x60, 0x84, 0x3c)
	formatted = append(formatted, 0x06) // wake-up type
	return smbiosStructure(1, formatted, "Micro-Star International Co., Ltd.", "MS-7C91", "2.0", "PF2X9K7Q")
}

func TestParseSMBIOSReadsSystemInformation(t *testing.T) {
	var table []byte
	table = append(table, smbiosStructure(0, []byte{1, 2, 0, 0}, "American Megatrends", "A.K1")...)
	table = append(table, typeOne(4)...)
	table = append(table, smbiosStructure(127, nil)...)

	got, err := ParseSMBIOS(RawSMBIOS{Major: 3, Minor: 3, Table: table})
	if err != nil {
		t.Fatalf("ParseSMBIOS: %v", err)
	}
	if got.Serial != "PF2X9K7Q" {
		t.Errorf("serial = %q, want PF2X9K7Q", got.Serial)
	}
	if got.UUID != "a2219e09-2c68-6d1d-a831-345a6060843c" {
		t.Errorf("uuid = %q, want the Windows rendering a2219e09-2c68-6d1d-a831-345a6060843c", got.UUID)
	}

	// Before SMBIOS 2.6 the UUID is read in network order.
	old, err := ParseSMBIOS(RawSMBIOS{Major: 2, Minor: 4, Table: table})
	if err != nil {
		t.Fatal(err)
	}
	if old.UUID != "099e21a2-682c-1d6d-a831-345a6060843c" {
		t.Errorf("2.4 uuid = %q, want network order", old.UUID)
	}
}

func TestParseSMBIOSAbsentSerialAndPlaceholderUUID(t *testing.T) {
	formatted := []byte{1, 2, 3, 0} // serial index 0: no serial
	formatted = append(formatted, make([]byte, 16)...)
	table := append(smbiosStructure(1, append(formatted, 0x06), "OEM", "Board", "1"), smbiosStructure(127, nil)...)
	got, err := ParseSMBIOS(RawSMBIOS{Major: 3, Table: table})
	if err != nil {
		t.Fatal(err)
	}
	if got.Serial != "" || got.UUID != "" {
		t.Fatalf("got %+v, want no serial and no UUID (an all-zero UUID is 'not present')", got)
	}
}

func TestParseSMBIOSRefusesATruncatedTable(t *testing.T) {
	if _, err := ParseSMBIOS(RawSMBIOS{Major: 3, Table: []byte{1, 40, 0, 0, 1, 2}}); err == nil {
		t.Fatal("a structure whose length runs past the table was accepted")
	}
	if _, err := ParseSMBIOS(RawSMBIOS{Major: 3, Table: smbiosStructure(127, nil)}); err == nil {
		t.Fatal("a table with no type 1 structure was accepted")
	}
}

func TestPlaceholderSerial(t *testing.T) {
	for _, s := range []string{"To be filled by O.E.M.", " Default string ", "0000000000", "System Serial Number", "", "XXXXXXXX", "N/A"} {
		if !PlaceholderSerial(s) {
			t.Errorf("%q is a placeholder", s)
		}
	}
	for _, s := range []string{"PF2X9K7Q", "5CG1234XYZ", "C02XK0AAJGH5"} {
		if PlaceholderSerial(s) {
			t.Errorf("%q is a real serial", s)
		}
	}
}

func TestCollectStatesWhatTheSourcesSay(t *testing.T) {
	reg := newFakeRegistry()
	reg.enrol("0A1B2C3D-0000-0000-0000-000000000001", intuneProvider, "9b1c0f2e-3c44-4b5e-9a61-2d7f0e8c1a55", 1)
	device := testCert(t, deviceCertIssuer, "5c2a3b1e-77d0-4e1f-9b6a-0c1d2e3f4a5b")
	reg.addKey(joinInfoKey + `\` + thumbprint(device))
	table := append(typeOne(4), smbiosStructure(127, nil)...)

	f := Collect(Sources{
		Registry:     reg,
		MachineCerts: func() ([]*x509.Certificate, error) { return []*x509.Certificate{device}, nil },
		SMBIOS:       func() (RawSMBIOS, error) { return RawSMBIOS{Major: 3, Minor: 3, Table: table}, nil },
		MachineID:    func() (string, error) { return "6E3E3C1B-2E8D-4C34-9E43-0D2C3B4A5968", nil },
	})
	want := protocol.DeviceAttestation{
		IntuneDeviceID: "9b1c0f2e-3c44-4b5e-9a61-2d7f0e8c1a55",
		EntraDeviceID:  "5c2a3b1e-77d0-4e1f-9b6a-0c1d2e3f4a5b",
		SerialNumber:   "PF2X9K7Q",
	}
	if f.Attestation != want {
		t.Fatalf("attestation = %+v, want %+v", f.Attestation, want)
	}
	if !f.Managed() {
		t.Fatal("an Intune enrolment was found but the device is not reported managed")
	}
	if f.HardwareSeed() != "smbios:a2219e09-2c68-6d1d-a831-345a6060843c/pf2x9k7q" {
		t.Fatalf("hardware seed = %q", f.HardwareSeed())
	}
}

func TestCollectOmitsAbsentFacts(t *testing.T) {
	f := Collect(Sources{
		Registry: newFakeRegistry(),
		SMBIOS:   func() (RawSMBIOS, error) { return RawSMBIOS{}, errors.New("firmware table unavailable") },
	})
	if f.AttestationOrNil() != nil {
		t.Fatalf("attestation = %+v, want none: nothing was stated", f.AttestationOrNil())
	}
	if f.Managed() {
		t.Fatal("no Intune enrolment, yet managed")
	}
	if f.HardwareSeed() != "" {
		t.Fatalf("hardware seed = %q from nothing", f.HardwareSeed())
	}
	if len(f.Notes) == 0 {
		t.Fatal("an unreadable firmware table left no note")
	}
}

// A placeholder serial is still the attestation serial (the MDM inventories the same field), but
// it never seeds an identity: every such machine would share it.
func TestPlaceholderSerialIsSentButNeverSeeds(t *testing.T) {
	formatted := append([]byte{1, 2, 3, 4}, make([]byte, 16)...)
	table := append(smbiosStructure(1, append(formatted, 6), "OEM", "Board", "1", "To be filled by O.E.M."), smbiosStructure(127, nil)...)
	f := Collect(Sources{
		SMBIOS:    func() (RawSMBIOS, error) { return RawSMBIOS{Major: 3, Table: table}, nil },
		MachineID: func() (string, error) { return "6E3E3C1B-2E8D-4C34-9E43-0D2C3B4A5968", nil },
	})
	if f.Attestation.SerialNumber != "To be filled by O.E.M." {
		t.Fatalf("serial = %q, want the firmware's value as stated", f.Attestation.SerialNumber)
	}
	if f.HardwareSeed() != "machine:6e3e3c1b-2e8d-4c34-9e43-0d2c3b4a5968" {
		t.Fatalf("hardware seed = %q, want the install id, never the placeholder", f.HardwareSeed())
	}
}
