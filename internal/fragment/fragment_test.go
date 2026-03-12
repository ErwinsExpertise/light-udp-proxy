package fragment

import "testing"

func TestIsFragmentedOOBEmpty(t *testing.T) {
	if IsFragmentedOOB(nil) {
		t.Fatal("expected false for empty oob")
	}
}
