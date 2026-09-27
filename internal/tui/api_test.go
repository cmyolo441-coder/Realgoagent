package tui

import "testing"

// A one-shot run has no prompt to answer, so file edits must be allowed. If
// they are not, the agent cannot do its job and is told the user refused —
// which never happened, because no user was asked.
func TestApprovalPolicyAllowsEditsWhenNonInteractive(t *testing.T) {
	approve := approvalPolicy(&Options{NonInteractive: true})
	for _, name := range []string{"write", "edit", "multi_edit", "patch"} {
		if !approve(name, map[string]any{"path": "a.go"}) {
			t.Errorf("%s denied in a non-interactive run; it should be allowed", name)
		}
	}
}

// Unattended runs must still refuse destructive shell. "No one is watching" is
// not consent to run `rm -rf`.
func TestApprovalPolicyRefusesDestructiveShellWhenNonInteractive(t *testing.T) {
	approve := approvalPolicy(&Options{NonInteractive: true})
	if approve("bash", map[string]any{"command": "rm -rf /tmp/whatever"}) {
		t.Error("destructive bash allowed in a non-interactive run")
	}
}

// Ordinary shell commands carry no risk pattern and stay allowed.
func TestApprovalPolicyAllowsSafeShellWhenNonInteractive(t *testing.T) {
	approve := approvalPolicy(&Options{NonInteractive: true})
	if !approve("bash", map[string]any{"command": "go test ./..."}) {
		t.Error("safe bash denied in a non-interactive run")
	}
}

// Interactively, edits need a human. The TUI rebinds the agent at startup and
// prompts, but the base policy must not wave them through.
func TestApprovalPolicyRequiresConfirmationInteractively(t *testing.T) {
	approve := approvalPolicy(&Options{})
	for _, name := range []string{"write", "edit", "multi_edit", "patch"} {
		if approve(name, map[string]any{"path": "a.go"}) {
			t.Errorf("%s allowed without confirmation in an interactive run", name)
		}
	}
	if approve("bash", map[string]any{"command": "git reset --hard"}) {
		t.Error("destructive bash allowed without confirmation")
	}
	if !approve("read", map[string]any{"path": "a.go"}) {
		t.Error("read denied; it never needs approval")
	}
}

// --accept-edits auto-approves file writes but never destructive shell.
func TestApprovalPolicyAcceptEdits(t *testing.T) {
	approve := approvalPolicy(&Options{AutoEdits: true})
	if !approve("write", map[string]any{"path": "a.go"}) {
		t.Error("write denied under --accept-edits")
	}
	if approve("bash", map[string]any{"command": "rm -rf build"}) {
		t.Error("destructive bash allowed under --accept-edits")
	}
}

// Plan mode is enforced by the read-only registry, but the policy must not
// contradict it if it is ever consulted.
func TestApprovalPolicyPlanModeDefersToRegistry(t *testing.T) {
	approve := approvalPolicy(&Options{PlanMode: true, NonInteractive: true})
	if !approve("write", map[string]any{"path": "a.go"}) {
		t.Error("policy should defer to the read-only registry rather than deny here")
	}
}
