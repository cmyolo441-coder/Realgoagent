package subagent

import (
	"context"
	"strings"
	"testing"

	"github.com/nova-ai/nova/internal/config"
	"github.com/nova-ai/nova/internal/tools"
)

func testConfig() *config.Config {
	c := config.Default()
	c.Providers = []config.Provider{{
		Name:    "fake",
		BaseURL: "http://127.0.0.1:0/v1",
		APIKey:  "x",
		Enabled: true,
		Models:  []config.Model{{ID: "m1", Alias: "m1"}},
	}}
	c.DefaultModel = "fake/m1"
	return c
}

func TestRolesAreListedStably(t *testing.T) {
	// The order is baked into the tool description the model sees. Map
	// iteration order would shuffle it between turns, which reads to a model
	// as the options changing.
	first := strings.Join(Roles(), ",")
	for i := 0; i < 20; i++ {
		if got := strings.Join(Roles(), ","); got != first {
			t.Fatalf("role order changed: %q vs %q", got, first)
		}
	}
	if !strings.Contains(first, RoleExplore) || !strings.Contains(first, RolePlan) || !strings.Contains(first, RoleGeneral) {
		t.Errorf("roles = %q, want all three", first)
	}
}

func TestEveryRoleHasADescription(t *testing.T) {
	// The description is the only thing telling the model what a role is for;
	// a blank one makes the choice guesswork.
	for _, r := range Roles() {
		if strings.TrimSpace(Describe(r)) == "" {
			t.Errorf("role %q has no description", r)
		}
	}
	if Describe("no-such-role") != "" {
		t.Error("an unknown role must not borrow another role's description")
	}
}

func TestReadOnlyRolesCannotWrite(t *testing.T) {
	// explore and plan are read-only by construction: the tools that mutate the
	// workspace must not be in their set at all, so the model is never even
	// offered them.
	for _, name := range []string{RoleExplore, RolePlan} {
		_, profile := lookup(name)
		if !profile.readOnly {
			t.Errorf("role %q must be read-only", name)
		}
		for _, tool := range profile.tools {
			switch tool {
			case "write", "edit", "multi_edit", "patch", "bash":
				t.Errorf("read-only role %q carries the mutating tool %q", name, tool)
			}
		}
	}
}

func TestGeneralRoleCanEdit(t *testing.T) {
	_, profile := lookup(RoleGeneral)
	if profile.readOnly {
		t.Error("the general role must be able to change files")
	}
	if len(profile.tools) == 0 {
		t.Error("the general role has no tools")
	}
}

func TestUnknownRoleFallsBackToExplore(t *testing.T) {
	// Falling back to the read-only default is the safe direction: an
	// unrecognised role must never gain the ability to write.
	name, profile := lookup("wizard")
	if name != DefaultRole {
		t.Errorf("name = %q, want %q", name, DefaultRole)
	}
	if !profile.readOnly {
		t.Error("the fallback role must be read-only")
	}
}

func TestRunRefusesAnEmptyTask(t *testing.T) {
	r := &Runner{Config: testConfig()}
	res := r.Run(context.Background(), Spawn{Role: RoleExplore, Task: "   "})
	if res.Err == nil {
		t.Fatal("an empty task must be refused")
	}
	if !strings.Contains(res.Err.Error(), "task is required") {
		t.Errorf("err = %v, want a missing-task error", res.Err)
	}
}

func TestRunRefusesPastTheDepthCap(t *testing.T) {
	// The cap is what stops a confused instruction from becoming a tree of
	// agents, so it has to hold at the runner and not only in the tool.
	r := &Runner{Config: testConfig()}
	res := r.Run(context.Background(), Spawn{Role: RoleExplore, Task: "do it", Depth: MaxDepth})
	if res.Err == nil {
		t.Fatal("a run past the cap must be refused")
	}
	if !strings.Contains(res.Err.Error(), "cannot spawn another subagent") {
		t.Errorf("err = %v, want a depth error", res.Err)
	}
}

func TestRunRefusesWithoutConfig(t *testing.T) {
	res := (&Runner{}).Run(context.Background(), Spawn{Role: RoleExplore, Task: "x"})
	if res.Err == nil {
		t.Fatal("a runner with no config must refuse rather than panic")
	}
}

func TestInstallPublishesTheRolesToTheTool(t *testing.T) {
	// The tool layer cannot import this package, so the catalogue travels
	// through Install. If that hand-off broke, the model would be offered a
	// role the runner does not have.
	Install(&Runner{Config: testConfig()})
	for _, r := range Roles() {
		if tools.RoleDescription(r) == "" {
			t.Errorf("role %q was not published to the task tool", r)
		}
	}
}

// TestMainAgentCanDelegate is the regression for an off-by-one that made the
// feature unreachable: the adapter passed the *incremented* depth to Run, so
// the child was compared against its own depth and every call was refused.
// The runner needs no reachable model for this — the refusal came back as an
// error, and a successful delegation gets past the cap check and fails later
// on the connection instead.
func TestMainAgentCanDelegate(t *testing.T) {
	r := &Runner{Config: testConfig()}
	var events []Event
	r.OnEvent = func(ev Event) { events = append(events, ev) }
	Install(r)
	res := r.Run(context.Background(), Spawn{Role: RoleExplore, Task: "look around"})
	if res.Err != nil && strings.Contains(res.Err.Error(), "cannot spawn another subagent") {
		t.Fatalf("the main agent must be able to delegate, got: %v", res.Err)
	}
	sawStart := false
	for _, ev := range events {
		if ev.Kind == EvSubStart {
			sawStart = true
		}
	}
	if !sawStart {
		t.Error("a permitted run must announce itself")
	}
}

func TestInflightReturnsToZero(t *testing.T) {
	before := Inflight()
	r := &Runner{Config: testConfig()}
	// The run fails fast (no reachable model), but it must still leave the
	// counter as it found it, or the view would show phantom agents forever.
	r.Run(context.Background(), Spawn{Role: RoleExplore, Task: "x"})
	if Inflight() != before {
		t.Errorf("inflight = %d, want %d", Inflight(), before)
	}
}
