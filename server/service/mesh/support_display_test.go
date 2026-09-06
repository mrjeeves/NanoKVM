package mesh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDisplayShowsTheSameSupportNumberAsAPI(t *testing.T) {
	b := &Bridge{state: LoadState(""), nodeID: "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd", joiningMesh: "custom-claim-mesh"}
	path := filepath.Join(t.TempDir(), "mesh_name")
	if err := os.WriteFile(path, []byte("cec-kvm-old-claim"), 0o644); err != nil {
		t.Fatal(err)
	}
	number := b.HelpStatus().SupportID
	if err := publishSupportNumber(path, number); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	displayed := strings.TrimSpace(string(content))
	if len(displayed) != 11 || strings.ReplaceAll(displayed, " ", "") != number {
		t.Fatalf("display %q does not match API number %q", displayed, number)
	}
	if displayed[3] != ' ' || displayed[7] != ' ' {
		t.Fatal("number is not grouped for reading aloud")
	}
	// 11 glyphs at 4px plus the 12px CEC label fit the PCIe's 63px row.
	if len(displayed)*4+12 > 63 {
		t.Fatal("support number overflows the PCIe display")
	}
	for _, invalid := range []string{"", "123", "cec-kvm-abcde-fghjk", "12345678x"} {
		if supportNumberDisplay(invalid) != "" {
			t.Fatalf("display accepted non-number %q", invalid)
		}
	}
	if supportNumberDisplay("001002003") != "001 002 003" {
		t.Fatal("leading zeroes lost")
	}
}
