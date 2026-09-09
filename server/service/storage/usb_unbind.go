package storage

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
)

// configfs returns ENODEV when asked to unbind a gadget that is already
// unbound. Recovery must continue to the bind step in that case. Read back
// the binding before accepting it; missing sysfs or a still-bound controller
// is a real error, not permission to hide a failed operation.
func unbindUSBGadget() error {
	err := usbWriteFile(usbGadgetUDC, []byte("\n"), 0o666)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.ENODEV) {
		data, readErr := usbReadFile(usbGadgetUDC)
		if readErr == nil && strings.TrimSpace(string(data)) == "" {
			return nil
		}
	}
	return fmt.Errorf("unbind USB gadget: %w", err)
}
