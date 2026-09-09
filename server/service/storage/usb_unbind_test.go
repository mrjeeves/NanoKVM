package storage

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestUnbindUSBGadgetAlreadyUnbound(t *testing.T) {
	for _, tt := range []struct {
		name     string
		writeErr error
		binding  string
		readErr  error
		wantErr  bool
	}{
		{"normal unbind", nil, "", nil, false},
		{"already unbound", syscall.ENODEV, "\n", nil, false},
		{"still bound", syscall.ENODEV, "8000000.dwc3\n", nil, true},
		{"missing gadget", syscall.ENODEV, "", os.ErrNotExist, true},
		{"permission error", os.ErrPermission, "", nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			oldRead, oldWrite := usbReadFile, usbWriteFile
			defer func() { usbReadFile, usbWriteFile = oldRead, oldWrite }()
			usbWriteFile = func(path string, data []byte, mode os.FileMode) error {
				if path != usbGadgetUDC || string(data) != "\n" {
					t.Fatalf("unexpected write: %s %q", path, data)
				}
				if tt.writeErr != nil {
					return &os.PathError{Op: "write", Path: path, Err: tt.writeErr}
				}
				return nil
			}
			usbReadFile = func(path string) ([]byte, error) {
				if path != usbGadgetUDC || !errors.Is(tt.writeErr, syscall.ENODEV) {
					t.Fatalf("unexpected read: %s", path)
				}
				return []byte(tt.binding), tt.readErr
			}
			err := unbindUSBGadget()
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr = %t", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, tt.writeErr) {
				t.Fatalf("lost original error: %v", err)
			}
		})
	}
}
