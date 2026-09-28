package tui

import "testing"

// Plan mode is the only gate left on a tool call, and it is enforced by the
// registry rather than by the UI. The two modes have to stay distinct values,
// or cmdMode would report a mode the registry never adopted.
func TestModeValuesAreDistinct(t *testing.T) {
	if ModeAgent == ModePlan {
		t.Fatal("ModePlan and ModeAgent are the same value; /mode would be a no-op")
	}
	if ModeAgent != 0 {
		t.Fatal("ModeAgent must be the zero value so it is the default")
	}
	if ModePlan == 0 {
		t.Fatal("ModePlan must not be the zero mode")
	}
}
