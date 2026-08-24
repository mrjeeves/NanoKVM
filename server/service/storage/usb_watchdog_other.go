//go:build !linux

package storage

// The USB gadget supervisor is Linux-only, like the gadget itself. The stubs
// keep the non-Linux build (developer machines) compiling.

func StartUSBWatchdog() {}

func noteUSBGadgetMutated() {}
