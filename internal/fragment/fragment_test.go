package fragment

import "testing"

func TestIsFragmentedIPv4(t *testing.T) {
	pkt := make([]byte, 20)
	pkt[0] = 0x45
	pkt[2] = 0x00
	pkt[3] = 0x14

	if IsFragmentedIPv4(pkt) {
		t.Fatal("expected non-fragment packet")
	}

	pkt[6] = 0x20 // MF flag
	if !IsFragmentedIPv4(pkt) {
		t.Fatal("expected MF fragment to be detected")
	}
}
