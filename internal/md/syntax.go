package md

import (
	"strings"
	"unicode"
)

// highlight applies a tiny built-in colouriser to a single source line.
// It is deliberately dependency-free: it is fast and good enough for the common
// languages (go, python, js/ts, rust, json, yaml, bash, sql).
func highlight(lang, line string) string {
	switch strings.ToLower(lang) {
	case "go":
		return highlightGo(line)
	case "js", "javascript", "ts", "typescript", "jsx", "tsx":
		return highlightJS(line)
	case "py", "python":
		return highlightPython(line)
	case "rs", "rust":
		return highlightRust(line)
	case "json":
		return highlightJSON(line)
	case "yaml", "yml", "toml":
		return highlightYAML(line)
	case "sh", "bash", "zsh", "shell", "console":
		return highlightShell(line)
	case "sql":
		return highlightShell(line)
	case "md", "markdown":
		return highlightMarkdown(line)
	}
	return tokens(line, cKeywords, cStrings, cComments, cNumbers, "\x1b[35m")
}

// token classes use ANSI 256 colours so they work over any background.
const (
	colKeyword  = "\x1b[38;5;176m"
	colString   = "\x1b[38;5;114m"
	colComment  = "\x1b[38;5;243m"
	colNumber   = "\x1b[38;5;179m"
	colType     = "\x1b[38;5;81m"
	colFunc     = "\x1b[38;5;221m"
	colOperator = "\x1b[38;5;250m"
	reset       = "\x1b[0m"
)

var goKeywords = []string{
	"break", "case", "chan", "const", "continue", "default", "defer", "else",
	"fallthrough", "for", "func", "go", "goto", "if", "import", "interface",
	"map", "package", "range", "return", "select", "struct", "switch", "type",
	"var", "nil", "true", "false", "iota", "string", "int", "bool", "byte",
	"rune", "float64", "error", "any",
}

var jsKeywords = []string{
	"const", "let", "var", "function", "return", "if", "else", "for", "while",
	"do", "switch", "case", "break", "continue", "new", "class", "extends",
	"this", "super", "import", "from", "export", "default", "await", "async",
	"try", "catch", "finally", "throw", "typeof", "instanceof", "delete", "in",
	"of", "yield", "static", "get", "set", "null", "undefined", "true", "false",
}

var pyKeywords = []string{
	"def", "class", "return", "if", "elif", "else", "for", "while", "break",
	"continue", "import", "from", "as", "with", "try", "except", "finally",
	"raise", "yield", "lambda", "pass", "global", "nonlocal", "del", "and",
	"or", "not", "in", "is", "assert", "async", "await", "True", "False", "None",
	"self", "cls",
}

var rsKeywords = []string{
	"as", "async", "await", "break", "const", "continue", "crate", "dyn",
	"else", "enum", "extern", "false", "fn", "for", "if", "impl", "in", "let",
	"loop", "match", "mod", "move", "mut", "pub", "ref", "return", "self",
	"Self", "static", "struct", "super", "trait", "true", "type", "unsafe",
	"use", "where", "while", "String", "Vec", "Option", "Result", "Ok", "Err",
	"Some", "None",
}

var yamlKeys = []string{"true", "false", "null", "yes", "no", "on", "off"}

var cKeywords = []string{
	"if", "else", "for", "while", "return", "int", "char", "float", "double",
	"void", "struct", "enum", "union", "sizeof", "static", "const", "unsigned",
	"true", "false", "NULL", "typedef",
}

var sqlKeywords = []string{
	"select", "from", "where", "insert", "into", "values", "update", "set",
	"delete", "create", "table", "drop", "alter", "index", "join", "left",
	"right", "inner", "outer", "on", "group", "by", "order", "having", "limit",
	"as", "and", "or", "not", "null", "primary", "key", "foreign", "references",
}

var shKeywords = []string{
	"if", "then", "else", "elif", "fi", "for", "in", "do", "done", "while",
	"case", "esac", "function", "return", "export", "local", "echo", "cd",
	"exit", "set", "source", "true", "false",
}

func highlightGo(line string) string { return tokens(line, goKeywords, true, true, true, colType) }
func highlightJS(line string) string { return tokens(line, jsKeywords, true, true, true, colType) }
func highlightPython(line string) string {
	if i := shCommentIndex(line); i >= 0 {
		return tokens(line[:i], pyKeywords, true, true, false, colType) + colComment + line[i:] + reset
	}
	return tokens(line, pyKeywords, true, true, false, colType)
}
func highlightRust(line string) string { return tokens(line, rsKeywords, true, true, true, colType) }

func highlightShell(line string) string {
	// Comment spans are coloured first and must not be re-scanned by tokens(),
	// so the comment is masked out with spaces and stitched back afterwards.
	if i := shCommentIndex(line); i >= 0 {
		head := tokens(line[:i], shKeywords, true, false, true, colOperator)
		return head + colComment + line[i:] + reset
	}
	return tokens(line, shKeywords, true, false, true, colOperator)
}

// shCommentIndex returns the index of a real comment start, or -1.
func shCommentIndex(line string) int {
	inStr := false
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inStr {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				inStr = false
			}
			continue
		}
		if c == '"' || c == '\'' {
			inStr = true
			quote = c
			continue
		}
		if c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == ';' || line[i-1] == '\t') {
			return i
		}
	}
	return -1
}
func highlightSQL(line string) string {
	return tokens(line, sqlKeywords, true, true, false, colKeyword)
}
func highlightYAML(line string) string {
	i := strings.Index(line, ":")
	if i < 0 {
		return line
	}
	key := line[:i+1]
	value := line[i+1:] // keep the raw remainder, including leading spaces
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return colType + key + reset + value
	}
	switch {
	case isYAMLKeyword(trimmed):
		value = colKeyword + value + reset
	case isNumeric(trimmed):
		value = colNumber + value + reset
	case strings.HasPrefix(trimmed, "\"") || strings.HasPrefix(trimmed, "'"):
		value = colString + value + reset
	default:
		value = colFunc + value + reset
	}
	return colType + key + reset + value
}

func highlightJSON(line string) string {
	return tokens(line, []string{"true", "false", "null"}, true, false, true, colKeyword)
}

func highlightMarkdown(line string) string {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") {
		return colKeyword + line + reset
	}
	if strings.HasPrefix(trimmed, "-") || strings.HasPrefix(trimmed, "*") {
		return colType + line + reset
	}
	return line
}

// highlightShComments colours a # comment, ignoring # inside strings and the
// shebang-style mid-line fragment markers.
func highlightShComments(line string) string {
	inStr := false
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inStr {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				inStr = false
			}
			continue
		}
		if c == '"' || c == '\'' {
			inStr = true
			quote = c
			continue
		}
		if c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == ';' || line[i-1] == '\t') {
			return line[:i] + colComment + line[i:] + reset
		}
	}
	return line
}

func highlightPyComments(line string) string {
	inStr := false
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inStr {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				inStr = false
			}
			continue
		}
		if c == '"' || c == '\'' {
			inStr = true
			quote = c
			continue
		}
		if c == '#' {
			return line[:i] + colComment + line[i:] + reset
		}
	}
	return line
}

func splitYAML(line string) (string, string) {
	i := strings.Index(line, ":")
	if i < 0 {
		return line, ""
	}
	key := line[:i+1]
	value := strings.TrimSpace(line[i+1:])
	if value == "" || strings.HasPrefix(value, "#") {
		return line, ""
	}
	if isYAMLKeyword(value) {
		value = colKeyword + value + reset
	} else if isNumeric(value) {
		value = colNumber + value + reset
	} else if strings.HasPrefix(value, "\"") || strings.HasPrefix(value, "'") {
		value = colString + value + reset
	} else {
		value = colFunc + value + reset
	}
	return colType + key + reset, value
}
func isYAMLKeyword(s string) bool {
	for _, k := range yamlKeys {
		if strings.EqualFold(s, k) {
			return true
		}
	}
	return false
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) && r != '.' && r != '-' && r != '+' {
			return false
		}
	}
	return true
}

// tokens colourises a line by scanning identifiers, strings, numbers and
// comments. It never reorders or drops characters.
func tokens(line string, keywords []string, doStrings, doComments, doNumbers bool, typeCol string) string {
	var b strings.Builder
	i := 0
	n := len(line)
	kw := func(w string) bool {
		for _, k := range keywords {
			if k == w {
				return true
			}
		}
		return false
	}
	for i < n {
		c := line[i]
		// line comments
		if doComments && (c == '/' && i+1 < n && line[i+1] == '/') {
			b.WriteString(colComment + line[i:] + reset)
			return b.String()
		}
		// block comment start (single line)
		if c == '/' && i+1 < n && line[i+1] == '*' {
			b.WriteString(colComment + line[i:] + reset)
			return b.String()
		}
		// strings
		if doStrings && (c == '"' || c == '\'' || c == '`') {
			j := i + 1
			for j < n {
				if line[j] == '\\' {
					j += 2
					continue
				}
				if line[j] == c {
					j++
					break
				}
				j++
			}
			if j > n {
				j = n
			}
			b.WriteString(colString + line[i:j] + reset)
			i = j
			continue
		}
		// numbers
		if doNumbers && (c >= '0' && c <= '9') {
			j := i
			for j < n && (line[j] >= '0' && line[j] <= '9' || line[j] == '.' ||
				line[j] == 'x' || line[j] == 'X' || (line[j] >= 'a' && line[j] <= 'f') ||
				(line[j] >= 'A' && line[j] <= 'F') || line[j] == '_') {
				j++
			}
			b.WriteString(colNumber + line[i:j] + reset)
			i = j
			continue
		}
		// identifiers / keywords
		if isIdentStart(c) {
			j := i
			for j < n && isIdentPart(line[j]) {
				j++
			}
			word := line[i:j]
			switch {
			case kw(word):
				b.WriteString(colKeyword + word + reset)
			case isTypeName(word) && typeCol != "":
				b.WriteString(typeCol + word + reset)
			case j < n && line[j] == '(':
				b.WriteString(colFunc + word + reset)
			default:
				b.WriteString(word)
			}
			i = j
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func isTypeName(w string) bool {
	if w == "" {
		return false
	}
	r0 := rune(w[0])
	return unicode.IsUpper(r0)
}

var cStrings = true
var cComments = true
var cNumbers = true
