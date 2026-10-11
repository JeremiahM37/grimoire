package exp

import "testing"

func TestSetTurnsExperimentsOnAndRestores(t *testing.T) {
	if On("header") {
		t.Fatal("no experiment should be on by default in tests")
	}
	restore := Set("Header", "ppr")
	if !On("header") || !On("ppr") || On("slot") {
		t.Fatal("Set did not select exactly the named experiments")
	}
	restore()
	if On("header") {
		t.Fatal("restore did not put the previous state back")
	}
}
