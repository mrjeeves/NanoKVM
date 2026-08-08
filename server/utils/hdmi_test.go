package utils

import (
	"os"
	"path/filepath"
	"testing"
)

func stageHdmi(t *testing.T) (flag, marker string) {
	t.Helper()
	dir := t.TempDir()
	flag, marker = filepath.Join(dir, "hdmi_disabled"), filepath.Join(dir, ".cleared")
	of, om := HDMIDisableFile, hdmiClearedMarker
	HDMIDisableFile, hdmiClearedMarker = flag, marker
	t.Cleanup(func() { HDMIDisableFile, hdmiClearedMarker = of, om })
	return flag, marker
}

// The device this exists for: switched off under the old behaviour, booting
// dark for good, with the fix being a file nobody knows to look for.
func TestClearHDMIDisabledOnceHealsADarkDevice(t *testing.T) {
	flag, marker := stageHdmi(t)
	if err := os.WriteFile(flag, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	ClearHDMIDisabledOnce()

	if IsHdmiDisabled() {
		t.Fatal("the disable flag survived")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the marker was not written, so this would run again")
	}
}

// Once, not every start. HDMI remains a setting, and re-enabling it on every
// restart would quietly make that setting a lie.
func TestClearHDMIDisabledOnceLeavesALaterChoiceAlone(t *testing.T) {
	flag, _ := stageHdmi(t)

	ClearHDMIDisabledOnce() // the migration, on a device that was never disabled

	// The operator turns HDMI off afterwards.
	PersistHDMIDisabled()
	if !IsHdmiDisabled() {
		t.Fatal("precondition: the flag should be set")
	}

	ClearHDMIDisabledOnce() // a later restart

	if !IsHdmiDisabled() {
		t.Fatal("a restart undid the operator's choice")
	}
	_ = flag
}

// A device we failed to heal must not be recorded as healed, or it never gets
// another attempt.
func TestClearHDMIDisabledOnceRetriesAfterAFailure(t *testing.T) {
	_, marker := stageHdmi(t)
	// A directory cannot be removed by os.Remove once it has content, so this
	// stands in for an unremovable flag.
	dir := HDMIDisableFile
	if err := os.MkdirAll(filepath.Join(dir, "child"), 0o755); err != nil {
		t.Fatal(err)
	}

	ClearHDMIDisabledOnce()

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a failed clear was recorded as done")
	}
}

// The ordinary case: nothing to clear, marker written, no noise.
func TestClearHDMIDisabledOnceIsQuietOnACleanDevice(t *testing.T) {
	_, marker := stageHdmi(t)

	ClearHDMIDisabledOnce()

	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the marker should be written even when there was nothing to clear")
	}
}
