package userhelper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/shadow-ai-capture/device/capture-core/localipc"
	"github.com/shadow-ai-capture/device/protocol"
)

// RunHelper is the helper's side of a connection to the service: it sends hello, then shows each
// notification the service asks for with show and answers whether it was shown, until the service
// closes the connection.
func RunHelper(conn net.Conn, hello protocol.HelperHello, show func(protocol.Notify) error) error {
	if err := localipc.WriteFrame(conn, message(protocol.TypeHelperHello, newID(), hello)); err != nil {
		return err
	}
	payload, err := localipc.ReadFrame(conn)
	if err != nil {
		return fmt.Errorf("the service did not answer helper_hello: %w", err)
	}
	var answer protocol.NativeMessage
	if err := json.Unmarshal(payload, &answer); err != nil {
		return fmt.Errorf("the service's answer to helper_hello: %w", err)
	}
	if answer.Type != protocol.TypeAck {
		var r protocol.Refusal
		_ = json.Unmarshal(answer.Body, &r)
		return fmt.Errorf("the service refused helper_hello: %s", r.Message)
	}
	for {
		payload, err := localipc.ReadFrame(conn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		var msg protocol.NativeMessage
		if json.Unmarshal(payload, &msg) != nil || msg.Type != protocol.TypeNotify {
			continue
		}
		var res protocol.NotifyResult
		var n protocol.Notify
		if err := json.Unmarshal(msg.Body, &n); err != nil {
			res.Error = fmt.Sprintf("notify body: %v", err)
		} else if err := n.Validate(); err != nil {
			res.Error = err.Error()
		} else if err := show(n); err != nil {
			res.Error = err.Error()
		} else {
			res.Shown = true
		}
		if err := localipc.WriteFrame(conn, message(protocol.TypeNotifyResult, msg.ID, res)); err != nil {
			return err
		}
	}
}
