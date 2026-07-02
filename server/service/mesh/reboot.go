package mesh

import (
	"fmt"
	"os/exec"
	"strings"
)

// restartDevice asks the OS to reboot this machine — the action behind an
// owner's AppControl restart_device. Mirrors AllMyStuff's node/src/reboot.rs:
// the reboot is asked of the OS, never forced from here; attempts run in order
// and the first acceptance wins; if all refuse, the caller gets every reason —
// a refusal must be visible, not a silent nothing-happened. Blocking (waits on
// each command), so call it off the event-stream goroutine.
//
// The attempt order covers both worlds this binary runs in: systemd on a dev
// box, busybox (`reboot`, `shutdown` maybe absent) on the SG2002 appliance.
func restartDevice() error {
	attempts := [][]string{
		{"systemctl", "reboot"},
		{"shutdown", "-r", "now"},
		{"reboot"},
	}
	var refusals []string
	for _, a := range attempts {
		if err := exec.Command(a[0], a[1:]...).Run(); err == nil {
			return nil
		} else {
			refusals = append(refusals, fmt.Sprintf("%s: %s", a[0], err))
		}
	}
	return fmt.Errorf("the OS refused the reboot (%s)", strings.Join(refusals, "; "))
}

// restartServer relaunches NanoKVM-Server onto the same build — AppControl
// "restart", one step lighter than a device reboot. On the appliance the init
// script owns the process, so we ask it to restart us; the child is detached
// (Start, not Run) because the restart kills this very process — waiting on it
// would be waiting on our own funeral. Confirmation is the fresh process's
// presence advert, exactly like the Rust node's sink.restart() path.
func restartServer() error {
	cmd := exec.Command("/etc/init.d/S95nanokvm", "restart")
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("/etc/init.d/S95nanokvm restart: %w", err)
	}
	// Detach: the child outlives us. Release rather than Wait so a zombie
	// isn't left if the init script somehow returns before killing us.
	return cmd.Process.Release()
}
