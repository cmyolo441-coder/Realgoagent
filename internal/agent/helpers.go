package agent

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"
)

// jsonUnmarshal tolerates code fences around JSON arguments.
func jsonUnmarshal(s string, v any) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	s = trimFence(s)
	if err := json.Unmarshal([]byte(s), v); err != nil {
		// try to repair single quotes / trailing commas
		return json.Unmarshal([]byte(repairJSON(s)), v)
	}
	return nil
}

func trimFence(s string) string {
	if len(s) >= 7 && s[:7] == "```json" {
		s = s[7:]
		if i := indexOf(s, "```"); i >= 0 {
			s = s[:i]
		}
	} else if len(s) >= 3 && s[:3] == "```" {
		s = s[3:]
		if i := indexOf(s, "```"); i >= 0 {
			s = s[:i]
		}
	}
	return strings.TrimSpace(s)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func repairJSON(s string) string {
	// minimal, best-effort: swap single quotes to double when there are no doubles yet
	if !strings.Contains(s, "\"") && strings.Contains(s, "'") {
		return strings.ReplaceAll(s, "'", "\"")
	}
	return s
}

func runShell(cmd, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Dir = dir
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
