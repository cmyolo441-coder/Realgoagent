package tools

import (
	"regexp"
)

// jsonSchemaHelpers is a placeholder reference so the compiler sees the helpers
// even when no tool in this file uses them.
var jsonSchemaHelpers = struct {
	compilePattern func(string, bool) (*regexp.Regexp, error)
}{compilePattern: compilePattern}

func compilePattern(pat string, ignoreCase bool) (*regexp.Regexp, error) {
	flags := ""
	if ignoreCase {
		flags = "(?i)"
	}
	return regexp.Compile(flags + pat)
}

// min/max are defined here so every tool file can use them.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// obj builds a JSON schema object of type object.
func obj(props ...map[string]any) map[string]any {
	schema := map[string]any{"type": "object", "properties": map[string]any{}}
	var required []string
	for _, p := range props {
		name, _ := p["name"].(string)
		if name == "" {
			continue
		}
		cp := map[string]any{}
		for k, v := range p {
			if k == "name" {
				continue
			}
			cp[k] = v
		}
		if cp["type"] == nil {
			cp["type"] = "string"
		}
		desc, _ := cp["description"].(string)
		typ, _ := cp["type"].(string)
		if desc == "" {
			cp["description"] = typ
		}
		schema["properties"].(map[string]any)[name] = cp
		if req, ok := p["required"].(bool); ok && req {
			required = append(required, name)
			delete(cp, "required")
		}
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// prop declares an optional property.
func prop(name, typ, desc string) map[string]any {
	return map[string]any{"name": name, "type": typ, "description": desc}
}

// req declares a required string property.
func req(name string, desc ...string) map[string]any {
	d := "The " + name + " parameter."
	if len(desc) > 0 && desc[0] != "" {
		d = desc[0]
	}
	return map[string]any{"name": name, "type": "string", "description": d, "required": true}
}
