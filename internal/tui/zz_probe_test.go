package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/nova-ai/nova/internal/md"
	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/util"
)

func TestProbeLook(t *testing.T) {
	p := theme.Get("nova")
	args := map[string]any{"command": "ls -la && go build ./...", "description": "List and build"}
	fmt.Printf("BRIEF = %q\n", util.Strip(toolCall(p, "bash", md.FormatArgs("bash", args))))

	fail := "---BRANCH---\n---LAST COMMITS---\n\n[exit 128]"
	fmt.Println("--- FAIL CASE ---")
	for _, r := range toolResult(p, "bash", "188ms", fail, true, 80) {
		fmt.Println(util.Strip(r))
	}

	ok := "total 8602\ndrwxr-xr-x  4 root root 3488 .\n" + strings.Repeat("x\n", 12)
	fmt.Println("--- OK CASE ---")
	for _, r := range toolResult(p, "bash", "8.844s", ok, false, 80) {
		fmt.Println(util.Strip(r))
	}
}
