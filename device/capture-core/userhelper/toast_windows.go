//go:build windows

package userhelper

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/go-ole/go-ole"

	"github.com/shadow-ai-capture/device/protocol"
)

// The Windows Runtime classes and interfaces a toast takes, with their method slots. Each
// interface's own methods follow IInspectable's six (QueryInterface, AddRef, Release, GetIids,
// GetRuntimeClassName, GetTrustLevel).
const (
	classXMLDocument  = "Windows.Data.Xml.Dom.XmlDocument"
	classToast        = "Windows.UI.Notifications.ToastNotification"
	classToastManager = "Windows.UI.Notifications.ToastNotificationManager"

	slotLoadXML                   = 6 // IXmlDocumentIO.LoadXml(HSTRING)
	slotCreateToastNotification   = 6 // IToastNotificationFactory.CreateToastNotification(IXmlDocument*, IToastNotification**)
	slotCreateToastNotifierWithID = 7 // IToastNotificationManagerStatics.CreateToastNotifierWithId(HSTRING, IToastNotifier**)
	slotShow                      = 6 // IToastNotifier.Show(IToastNotification*)

	roInitMultithreaded = 1
	sFalse              = 1
	rpcEChangedMode     = 0x80010106
)

var (
	iidXMLDocument                    = ole.NewGUID("{F7F3A506-1E87-42D6-BCFB-B8C809FA5494}")
	iidXMLDocumentIO                  = ole.NewGUID("{6CD0E74E-EE65-4489-9EBF-CA43E87BA637}")
	iidToastNotificationFactory       = ole.NewGUID("{04124B20-82C6-4229-B109-FD9ED4662B53}")
	iidToastNotificationManagerStatic = ole.NewGUID("{50AC103F-D235-4598-BBEF-98FE4D1A3AD4}")
)

// Toaster shows toasts under appID. The Windows Runtime is initialised on one OS thread for the
// toaster's life, and every toast is shown from that thread.
type Toaster struct {
	appID string
	reqs  chan toastRequest
}

type toastRequest struct {
	n    protocol.Notify
	done chan error
}

// NewToaster starts the toaster's thread.
func NewToaster(appID string) (*Toaster, error) {
	t := &Toaster{appID: appID, reqs: make(chan toastRequest)}
	ready := make(chan error, 1)
	go func() {
		// The thread stays locked for the process's life: it owns the apartment.
		runtime.LockOSThread()
		if err := ole.RoInitialize(roInitMultithreaded); err != nil && !hresultIs(err, sFalse, rpcEChangedMode) {
			ready <- fmt.Errorf("RoInitialize: %w", err)
			return
		}
		ready <- nil
		for req := range t.reqs {
			req.done <- t.show(req.n)
		}
	}()
	if err := <-ready; err != nil {
		return nil, err
	}
	return t, nil
}

// Show shows one toast.
func (t *Toaster) Show(n protocol.Notify) error {
	done := make(chan error, 1)
	t.reqs <- toastRequest{n: n, done: done}
	return <-done
}

func (t *Toaster) show(n protocol.Notify) error {
	doc, err := ole.RoActivateInstance(classXMLDocument)
	if err != nil {
		return fmt.Errorf("activating %s: %w", classXMLDocument, err)
	}
	defer doc.Release()
	docIO, err := queryInterface(doc, iidXMLDocumentIO)
	if err != nil {
		return fmt.Errorf("IXmlDocumentIO: %w", err)
	}
	defer docIO.Release()
	// The document is ASCII (toastXML), so its length in characters is its length in UTF-16 units,
	// which is what WindowsCreateString takes.
	xml, err := ole.NewHString(toastXML(n))
	if err != nil {
		return fmt.Errorf("WindowsCreateString: %w", err)
	}
	defer ole.DeleteHString(xml)
	if err := call(docIO, slotLoadXML, uintptr(xml)); err != nil {
		return fmt.Errorf("IXmlDocumentIO.LoadXml: %w", err)
	}
	xmlDoc, err := queryInterface(doc, iidXMLDocument)
	if err != nil {
		return fmt.Errorf("IXmlDocument: %w", err)
	}
	defer xmlDoc.Release()

	factory, err := ole.RoGetActivationFactory(classToast, iidToastNotificationFactory)
	if err != nil {
		return fmt.Errorf("activation factory of %s: %w", classToast, err)
	}
	defer factory.Release()
	var toast *ole.IInspectable
	if err := call(factory, slotCreateToastNotification, uintptr(unsafe.Pointer(xmlDoc)), uintptr(unsafe.Pointer(&toast))); err != nil {
		return fmt.Errorf("IToastNotificationFactory.CreateToastNotification: %w", err)
	}
	defer toast.Release()

	manager, err := ole.RoGetActivationFactory(classToastManager, iidToastNotificationManagerStatic)
	if err != nil {
		return fmt.Errorf("activation factory of %s: %w", classToastManager, err)
	}
	defer manager.Release()
	appID, err := ole.NewHString(t.appID)
	if err != nil {
		return fmt.Errorf("WindowsCreateString: %w", err)
	}
	defer ole.DeleteHString(appID)
	var notifier *ole.IInspectable
	if err := call(manager, slotCreateToastNotifierWithID, uintptr(appID), uintptr(unsafe.Pointer(&notifier))); err != nil {
		return fmt.Errorf("IToastNotificationManagerStatics.CreateToastNotifierWithId(%s): %w", t.appID, err)
	}
	defer notifier.Release()
	if err := call(notifier, slotShow, uintptr(unsafe.Pointer(toast))); err != nil {
		return fmt.Errorf("IToastNotifier.Show: %w", err)
	}
	return nil
}

// queryInterface asks obj for the interface iid.
func queryInterface(obj *ole.IInspectable, iid *ole.GUID) (*ole.IInspectable, error) {
	disp, err := obj.QueryInterface(iid)
	if err != nil {
		return nil, err
	}
	return (*ole.IInspectable)(unsafe.Pointer(disp)), nil
}

// call invokes the method in slot of obj's vtable with obj as its first argument and fails on a
// failing HRESULT.
func call(obj *ole.IInspectable, slot int, args ...uintptr) error {
	vtbl := (*[16]uintptr)(unsafe.Pointer(obj.RawVTable))
	hr, _, _ := syscall.SyscallN(vtbl[slot], append([]uintptr{uintptr(unsafe.Pointer(obj))}, args...)...)
	if int32(hr) < 0 {
		return ole.NewError(hr)
	}
	return nil
}

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
