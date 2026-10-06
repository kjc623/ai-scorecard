// Package hostinfo reads what the operating system states about this device and about the person
// at it: the MDM and directory identifiers an enrolment is checked against (attestation), a
// hardware seed for the enrolment idempotency key, and the interactive console user a user_ref is
// derived from.
//
// Every reader sits behind a seam (Registry, the machine certificate list, the raw SMBIOS table,
// the user lookups) so the selection rules run on any OS in a test; only the windows files touch
// the real system. A fact the system does not state is left empty. Nothing here guesses, because
// the server compares these values with the customer's MDM, and a guess would either refuse a
// managed device or vouch for an unmanaged one.
package hostinfo

import (
	"crypto/sha1"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/shadow-ai-capture/device/protocol"
)

// ErrNotFound reports an absent registry key or value. It is an absent fact, not a failure.
var ErrNotFound = errors.New("hostinfo: not found")

// Registry is the read-only view of HKEY_LOCAL_MACHINE the readers need. Paths are relative to
// HKLM and separated by backslashes. An absent key or value is ErrNotFound.
type Registry interface {
	SubKeys(path string) ([]string, error)
	String(path, name string) (string, error)
	Integer(path, name string) (uint64, error)
}

// RawSMBIOS is the firmware's SMBIOS structure table and the version it declares, as Windows'
// GetSystemFirmwareTable('RSMB') returns it.
type RawSMBIOS struct {
	Major, Minor byte
	Table        []byte
}

// Sources are the seams Collect reads. A nil source is an absent fact, which is what every
// platform without that facility reports.
type Sources struct {
	Registry Registry
	// MachineCerts lists the certificates in the machine's personal store (Windows LocalMachine\My).
	MachineCerts func() ([]*x509.Certificate, error)
	SMBIOS       func() (RawSMBIOS, error)
	// MachineID is the operating system's install identifier (Windows MachineGuid, Linux
	// /etc/machine-id). It changes on re-image, so it is the last hardware seed, not the first.
	MachineID func() (string, error)
}

// Facts is what Collect found. Notes say why a fact is absent or was set aside, for --print-config
// and the log; they never contain a guessed value.
type Facts struct {
	Attestation protocol.DeviceAttestation
	// SystemUUID is the SMBIOS system UUID, when the firmware states a real one.
	SystemUUID string
	// serialIsReal is false when the firmware's serial is an OEM placeholder: still sent as the
	// attestation serial (the MDM inventories the same field), never used as a hardware seed.
	serialIsReal bool
	MachineID    string
	Notes        []string
}

// Managed reports whether an Intune enrolment was found. The absence of one is not evidence the
// device is unmanaged (another MDM, ConfigMgr or GPO may manage it), so callers report 'unknown'.
func (f Facts) Managed() bool { return f.Attestation.IntuneDeviceID != "" }

// AttestationOrNil returns the attestation, or nil when the system stated none of it, so the
// enrolment body omits the object rather than sending an empty one.
func (f Facts) AttestationOrNil() *protocol.DeviceAttestation {
	if f.Attestation == (protocol.DeviceAttestation{}) {
		return nil
	}
	a := f.Attestation
	return &a
}

// HardwareSeed is the preferred seed for the enrolment idempotency key: what survives a
// re-image first. The SMBIOS UUID and serial are the hardware's; the MDM and directory ids are
// re-issued by a re-enrolment or a re-join, so they are not used; the install id changes on
// re-image but at least never collides. Empty means the system stated none of them.
func (f Facts) HardwareSeed() string {
	switch {
	case f.SystemUUID != "" && f.serialIsReal:
		return "smbios:" + f.SystemUUID + "/" + strings.ToLower(f.Attestation.SerialNumber)
	case f.SystemUUID != "":
		return "smbios:" + f.SystemUUID
	case f.serialIsReal:
		return "serial:" + strings.ToLower(f.Attestation.SerialNumber)
	case f.MachineID != "":
		return "machine:" + strings.ToLower(f.MachineID)
	default:
		return ""
	}
}

// Collect reads every fact the sources can state. It never fails: an unreadable source is an absent
// fact with a note.
func Collect(src Sources) Facts {
	var f Facts
	if src.Registry != nil {
		id, note := IntuneDeviceID(src.Registry)
		f.Attestation.IntuneDeviceID = id
		f.note(note)
	}
	var certs []*x509.Certificate
	if src.MachineCerts != nil {
		var err error
		if certs, err = src.MachineCerts(); err != nil {
			f.note(fmt.Sprintf("machine certificate store unreadable: %v", err))
		}
	}
	if src.Registry != nil || len(certs) > 0 {
		id, note := EntraDeviceID(src.Registry, certs)
		f.Attestation.EntraDeviceID = id
		f.note(note)
	}
	if src.SMBIOS != nil {
		raw, err := src.SMBIOS()
		if err != nil {
			f.note(fmt.Sprintf("SMBIOS table unreadable: %v", err))
		} else if sys, err := ParseSMBIOS(raw); err != nil {
			f.note(fmt.Sprintf("SMBIOS table unparseable: %v", err))
		} else {
			f.Attestation.SerialNumber = sys.Serial
			f.serialIsReal = sys.Serial != "" && !PlaceholderSerial(sys.Serial)
			f.SystemUUID = sys.UUID
			if sys.Serial != "" && !f.serialIsReal {
				f.note(fmt.Sprintf("SMBIOS serial %q is an OEM placeholder; sent as stated, not used as a hardware seed", sys.Serial))
			}
		}
	}
	if src.MachineID != nil {
		if id, err := src.MachineID(); err == nil {
			f.MachineID = strings.TrimSpace(id)
		}
	}
	return f
}

func (f *Facts) note(s string) {
	if s != "" {
		f.Notes = append(f.Notes, s)
	}
}

// The Windows locations the readers use. They are the documented MDM and join-state locations,
// stated once here so a test pins the same paths production reads.
const (
	enrollmentsKey = `SOFTWARE\Microsoft\Enrollments`
	// intuneProvider is the ProviderID Windows records for an Intune (MDM) enrolment; the same
	// string names the DMClient subkey that holds the enrolment's EntDMID.
	intuneProvider = "MS DM Server"
	joinInfoKey    = `SYSTEM\CurrentControlSet\Control\CloudDomainJoin\JoinInfo`
	// deviceCertIssuer is the issuer of the device certificate Entra join (and hybrid join) puts in
	// LocalMachine\My; its subject CN is the Entra device id.
	deviceCertIssuer = "MS-Organization-Access"
	identityStoreKey = `SOFTWARE\Microsoft\IdentityStore\Cache`
	cryptographyKey  = `SOFTWARE\Microsoft\Cryptography`
)

// IntuneDeviceID returns the Intune managed-device id: the EntDMID of the enrolment whose
// ProviderID is "MS DM Server". Enrolment keys linger after an unenrolment, so an active one
// (EnrollmentState 1) is preferred, and two different ids among the candidates are reported as
// ambiguous rather than resolved by picking one.
func IntuneDeviceID(reg Registry) (string, string) {
	keys, err := reg.SubKeys(enrollmentsKey)
	if errors.Is(err, ErrNotFound) {
		return "", ""
	}
	if err != nil {
		return "", fmt.Sprintf("enrolments unreadable: %v", err)
	}
	var active, any []string
	for _, k := range keys {
		base := enrollmentsKey + `\` + k
		if pid, err := reg.String(base, "ProviderID"); err != nil || strings.TrimSpace(pid) != intuneProvider {
			continue
		}
		id, err := reg.String(base+`\DMClient\`+intuneProvider, "EntDMID")
		id = strings.TrimSpace(id)
		if err != nil || id == "" {
			continue
		}
		any = appendDistinct(any, id)
		if state, err := reg.Integer(base, "EnrollmentState"); err == nil && state == 1 {
			active = appendDistinct(active, id)
		}
	}
	candidates := any
	if len(active) > 0 {
		candidates = active
	}
	switch len(candidates) {
	case 0:
		return "", ""
	case 1:
		return candidates[0], ""
	default:
		return "", fmt.Sprintf("%d Intune enrolments name different EntDMIDs; none is sent", len(candidates))
	}
}

// EntraDeviceID returns the Entra device id. The join state names the device certificate by its
// thumbprint (JoinInfo\<thumbprint>), and that certificate, issued by MS-Organization-Access, carries
// the device id as its subject CN. With no join state, a single MS-Organization-Access certificate
// is still the device's; more than one is ambiguous and nothing is sent.
func EntraDeviceID(reg Registry, certs []*x509.Certificate) (string, string) {
	byThumb := map[string]string{}
	var orgIDs []string
	for _, c := range certs {
		id, ok := deviceCertID(c)
		if !ok {
			continue
		}
		sum := sha1.Sum(c.Raw)
		byThumb[strings.ToUpper(hex.EncodeToString(sum[:]))] = id
		orgIDs = appendDistinct(orgIDs, id)
	}
	var thumbs []string
	if reg != nil {
		var err error
		thumbs, err = reg.SubKeys(joinInfoKey)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return "", fmt.Sprintf("join state unreadable: %v", err)
		}
	}
	if len(thumbs) > 0 {
		var joined []string
		for _, t := range thumbs {
			if id, ok := byThumb[strings.ToUpper(strings.TrimSpace(t))]; ok {
				joined = appendDistinct(joined, id)
			}
		}
		switch len(joined) {
		case 1:
			return joined[0], ""
		case 0:
			return "", "the device is joined but its join certificate is not in LocalMachine\\My"
		default:
			return "", fmt.Sprintf("%d join certificates name different device ids; none is sent", len(joined))
		}
	}
	switch len(orgIDs) {
	case 0:
		return "", ""
	case 1:
		return orgIDs[0], ""
	default:
		return "", fmt.Sprintf("%d %s certificates name different device ids and no join state picks one; none is sent", len(orgIDs), deviceCertIssuer)
	}
}

// deviceCertID returns the Entra device id a certificate carries, when it is a device certificate.
func deviceCertID(c *x509.Certificate) (string, bool) {
	if c == nil || c.Issuer.CommonName != deviceCertIssuer {
		return "", false
	}
	cn := strings.TrimSpace(c.Subject.CommonName)
	if !guidPattern.MatchString(cn) {
		return "", false
	}
	return strings.ToLower(cn), true
}

// MachineGUID reads Windows' install identifier.
func MachineGUID(reg Registry) (string, error) {
	return reg.String(cryptographyKey, "MachineGuid")
}

var guidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func appendDistinct(list []string, v string) []string {
	for _, have := range list {
		if strings.EqualFold(have, v) {
			return list
		}
	}
	list = append(list, v)
	sort.Strings(list)
	return list
}
