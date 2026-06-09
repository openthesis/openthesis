// Minimal YAML subset parser for docker-compose files.
//
// This parser handles the subset of YAML that Docker Compose files use in
// practice: scalar key/value pairs, nested mappings, block lists ("- foo"),
// flow collections ("[a, b]" and "{k: v}"), and quoted strings. It does NOT
// implement the full YAML spec; no anchors/aliases, no multi-document
// streams, no tag directives, no complex flow scalars, no folding markers.
//
// The strategy is to build a tree of any / map[string]any / []any values
// from the YAML input, then marshal that tree back to JSON and re-use the
// existing JSON compose parser. This keeps the YAML-specific code small and
// inherits every parsing quirk we already handle on the JSON path for free.
package container

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// yamlLine represents a single non-blank line from the input, stripped of
// its comment and with its indentation depth computed in spaces.
type yamlLine struct {
	indent  int
	content string
	number  int // 1-based line number for errors
}

// tokenizeYAML splits raw input into logical lines, discarding blank lines,
// full-line comments, and trailing comments. Tabs are rejected because they
// confuse indentation-based parsing in the same way YAML itself rejects them.
func tokenizeYAML(data []byte) ([]yamlLine, error) {
	var lines []yamlLine
	for i, rawLine := range strings.Split(string(data), "\n") {
		lineNum := i + 1
		// Strip \r from CRLF files.
		line := strings.TrimRight(rawLine, "\r")
		// Strip a trailing comment, but only when the '#' is preceded by
		// whitespace or at column 0; a '#' inside a quoted string or URL
		// must survive.
		if idx := findCommentStart(line); idx >= 0 {
			line = line[:idx]
		}
		// Skip blank lines.
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Tab indentation is ambiguous; reject early with a clear message.
		if len(line) > 0 && line[0] == '\t' {
			return nil, fmt.Errorf("yaml line %d: tab indentation is not supported; use spaces", lineNum)
		}
		indent := 0
		for indent < len(line) && line[indent] == ' ' {
			indent++
		}
		content := strings.TrimRight(line[indent:], " ")
		if content == "" {
			continue
		}
		// Ignore YAML document markers that bracket single-doc streams.
		if content == "---" || content == "..." {
			continue
		}
		lines = append(lines, yamlLine{indent: indent, content: content, number: lineNum})
	}
	return lines, nil
}

// findCommentStart returns the byte index of the '#' that begins a trailing
// comment on this line, or -1 if none exists. It respects single and double
// quoted strings so that URLs and scalar values containing '#' are preserved.
func findCommentStart(line string) int {
	var inSingle, inDouble bool
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && inDouble && i+1 < len(line):
			i++ // skip escaped char
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '#' && !inSingle && !inDouble:
			// A comment must be at column 0 or preceded by whitespace.
			if i == 0 || line[i-1] == ' ' || line[i-1] == '\t' {
				return i
			}
		}
	}
	return -1
}

// ParseYAML parses a minimal YAML subset into a generic tree. The returned
// value is nil, a string, a []any, or a map[string]any. Numbers are returned
// as strings to avoid lossy float conversions; docker-compose fields are
// consumed as strings anyway.
func parseYAMLTree(data []byte) (any, error) {
	lines, err := tokenizeYAML(data)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return map[string]any{}, nil
	}
	p := &yamlParser{lines: lines}
	return p.parseBlock(0)
}

type yamlParser struct {
	lines []yamlLine
	pos   int
}

func (p *yamlParser) peek() (yamlLine, bool) {
	if p.pos >= len(p.lines) {
		return yamlLine{}, false
	}
	return p.lines[p.pos], true
}

// parseBlock parses a contiguous set of lines whose indentation is exactly
// `indent`. It auto-detects whether the block is a mapping (`key: value`) or
// a sequence (`- item`) from the first line and delegates accordingly.
func (p *yamlParser) parseBlock(indent int) (any, error) {
	line, ok := p.peek()
	if !ok {
		return nil, nil
	}
	if line.indent < indent {
		return nil, nil
	}
	if strings.HasPrefix(line.content, "- ") || line.content == "-" {
		return p.parseSeq(indent)
	}
	return p.parseMap(indent)
}

// parseMap reads successive "key: value" lines at the given indent.
func (p *yamlParser) parseMap(indent int) (map[string]any, error) {
	result := make(map[string]any)
	for {
		line, ok := p.peek()
		if !ok || line.indent < indent {
			return result, nil
		}
		if line.indent > indent {
			return nil, fmt.Errorf("yaml line %d: unexpected indentation (got %d, expected %d)", line.number, line.indent, indent)
		}
		// A sequence marker at this level means the caller should have
		// dispatched to parseSeq. Stop and let them handle it.
		if strings.HasPrefix(line.content, "- ") || line.content == "-" {
			return result, nil
		}
		key, rest, err := splitKey(line.content)
		if err != nil {
			return nil, fmt.Errorf("yaml line %d: %w", line.number, err)
		}
		p.pos++

		// Empty value => nested block at a deeper indent.
		if rest == "" {
			child, err := p.parseNestedBlock(indent)
			if err != nil {
				return nil, err
			}
			result[key] = child
			continue
		}

		// Inline scalar / flow value.
		val, err := parseScalar(rest)
		if err != nil {
			return nil, fmt.Errorf("yaml line %d: %w", line.number, err)
		}
		result[key] = val
	}
}

// parseNestedBlock reads a child block that must be indented strictly deeper
// than `parent`. It returns nil if the next line doesn't satisfy that.
func (p *yamlParser) parseNestedBlock(parent int) (any, error) {
	line, ok := p.peek()
	if !ok || line.indent <= parent {
		return nil, nil
	}
	return p.parseBlock(line.indent)
}

// parseSeq reads successive "- item" lines at the given indent. Items may be
// inline scalars, nested mappings (the "- key: val" shortcut), or nested
// sequences.
func (p *yamlParser) parseSeq(indent int) ([]any, error) {
	var result []any
	for {
		line, ok := p.peek()
		if !ok || line.indent < indent {
			return result, nil
		}
		if line.indent > indent {
			return nil, fmt.Errorf("yaml line %d: unexpected indentation (got %d, expected %d)", line.number, line.indent, indent)
		}
		if !strings.HasPrefix(line.content, "-") {
			return result, nil
		}
		// Strip the leading "-" and any single space after it.
		item := strings.TrimPrefix(line.content, "-")
		item = strings.TrimPrefix(item, " ")
		p.pos++

		// Bare dash: the next line at a deeper indent is the item value.
		if item == "" {
			child, err := p.parseNestedBlock(indent)
			if err != nil {
				return nil, err
			}
			result = append(result, child)
			continue
		}

		// Inline "- key: value" introduces a mapping whose first key lives on
		// the same line as the dash. Additional keys follow at the same
		// absolute column (dash + space + key position).
		if key, rest, err := tryMappingStart(item); err == nil && key != "" {
			sub := make(map[string]any)
			if rest == "" {
				child, err := p.parseNestedBlock(indent + 2)
				if err != nil {
					return nil, err
				}
				sub[key] = child
			} else {
				val, err := parseScalar(rest)
				if err != nil {
					return nil, fmt.Errorf("yaml line %d: %w", line.number, err)
				}
				sub[key] = val
			}
			// Absorb further map entries at the deeper indent column used
			// by the virtual mapping ("- " consumes two columns).
			childIndent := indent + 2
			for {
				next, ok := p.peek()
				if !ok || next.indent != childIndent {
					break
				}
				if strings.HasPrefix(next.content, "- ") || next.content == "-" {
					break
				}
				k2, rest2, err := splitKey(next.content)
				if err != nil {
					return nil, fmt.Errorf("yaml line %d: %w", next.number, err)
				}
				p.pos++
				if rest2 == "" {
					child, err := p.parseNestedBlock(childIndent)
					if err != nil {
						return nil, err
					}
					sub[k2] = child
				} else {
					val, err := parseScalar(rest2)
					if err != nil {
						return nil, fmt.Errorf("yaml line %d: %w", next.number, err)
					}
					sub[k2] = val
				}
			}
			result = append(result, sub)
			continue
		}

		// Inline scalar value.
		val, err := parseScalar(item)
		if err != nil {
			return nil, fmt.Errorf("yaml line %d: %w", line.number, err)
		}
		result = append(result, val)
	}
}

// splitKey parses "key: value" or "key:" from a line. Returns the unquoted
// key and the raw value portion (which may be empty).
func splitKey(line string) (string, string, error) {
	// Walk the line respecting quotes to find the key-terminating ':'.
	var inSingle, inDouble bool
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && inDouble && i+1 < len(line):
			i++
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == ':' && !inSingle && !inDouble:
			// The colon ends the key only if it is followed by end-of-line
			// or whitespace. "key:value" without a space is treated as a
			// bare scalar key lookup per YAML spec.
			if i+1 == len(line) || line[i+1] == ' ' || line[i+1] == '\t' {
				key := strings.TrimSpace(line[:i])
				rest := strings.TrimSpace(line[i+1:])
				if key == "" {
					return "", "", fmt.Errorf("empty key")
				}
				return unquote(key), rest, nil
			}
		}
	}
	return "", "", fmt.Errorf("expected ':' in mapping line %q", line)
}

// tryMappingStart attempts to parse the leading "key:" of a mapping entry,
// returning the key and the remainder of the line. It returns a non-nil
// error if the content looks like a plain scalar rather than a mapping.
func tryMappingStart(line string) (string, string, error) {
	// Lines starting with '{' or '[' are flow collections, not mappings.
	if strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[") {
		return "", "", fmt.Errorf("flow collection")
	}
	return splitKey(line)
}

// parseScalar turns an inline value ("foo", "'a b'", "[1, 2]", "{k: v}", 42)
// into the appropriate Go value. Plain scalars are returned as strings.
func parseScalar(s string) (any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	// Flow mapping.
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		return parseFlow(s)
	}
	// Flow sequence.
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		return parseFlow(s)
	}
	// Quoted scalar.
	if (strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) && len(s) >= 2) ||
		(strings.HasPrefix(s, `'`) && strings.HasSuffix(s, `'`) && len(s) >= 2) {
		return unquote(s), nil
	}
	// Null-like tokens that compose files occasionally use.
	switch s {
	case "null", "~", "Null", "NULL":
		return nil, nil
	}
	// Booleans and numbers: keep them as strings. Compose fields are all
	// string-valued to the downstream parser, and stringifying avoids any
	// int-vs-float foot-guns (e.g. "version: 3" ≠ version: "3").
	return s, nil
}

// parseFlow parses a flow collection ("[a, b]" or "{k: v, k2: v2}") using
// the standard library's JSON parser after translating YAML flow syntax to
// equivalent JSON. The translation handles unquoted scalars, single-quoted
// strings, and bare keys.
func parseFlow(s string) (any, error) {
	jsonSrc, err := flowToJSON(s)
	if err != nil {
		return nil, err
	}
	var out any
	dec := json.NewDecoder(strings.NewReader(jsonSrc))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("flow %q: %w", s, err)
	}
	return normalizeJSONValue(out), nil
}

// normalizeJSONValue converts json.Number values into strings so the final
// tree matches what block-style parsing produces. This keeps every field
// consumed by the compose JSON parser in a consistent shape.
func normalizeJSONValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			x[k] = normalizeJSONValue(vv)
		}
		return x
	case []any:
		for i, vv := range x {
			x[i] = normalizeJSONValue(vv)
		}
		return x
	case json.Number:
		return x.String()
	default:
		return x
	}
}

// flowToJSON rewrites a YAML flow collection into valid JSON. The rewrite
// walks character by character, tracking nesting and quote state, and wraps
// bare scalars in double quotes. It is intentionally small; flow collections
// in compose files are nearly always short shorthand lists like
// `["CMD", "curl", "-f", "http://localhost/"]`.
func flowToJSON(s string) (string, error) {
	var out strings.Builder
	out.Grow(len(s) + 8)
	i := 0
	// expectKey is true when the next non-whitespace token should be treated
	// as a mapping key (which in YAML flow mappings may be bare).
	depthStack := []byte{}
	for i < len(s) {
		c := s[i]
		switch c {
		case ' ', '\t':
			out.WriteByte(c)
			i++
		case '{', '[':
			out.WriteByte(c)
			depthStack = append(depthStack, c)
			i++
		case '}', ']':
			out.WriteByte(c)
			if len(depthStack) > 0 {
				depthStack = depthStack[:len(depthStack)-1]
			}
			i++
		case ',', ':':
			out.WriteByte(c)
			i++
		case '"':
			// Copy a double-quoted scalar verbatim.
			end := i + 1
			for end < len(s) {
				if s[end] == '\\' && end+1 < len(s) {
					end += 2
					continue
				}
				if s[end] == '"' {
					end++
					break
				}
				end++
			}
			out.WriteString(s[i:end])
			i = end
		case '\'':
			// Translate a single-quoted scalar into a JSON string.
			end := i + 1
			for end < len(s) && s[end] != '\'' {
				end++
			}
			if end >= len(s) {
				return "", fmt.Errorf("unterminated single quote in flow %q", s)
			}
			lit := s[i+1 : end]
			encoded, err := json.Marshal(lit)
			if err != nil {
				return "", err
			}
			out.Write(encoded)
			i = end + 1
		default:
			// Bare scalar: read until the next structural character.
			end := i
			for end < len(s) && !isFlowStructural(s[end]) {
				end++
			}
			token := strings.TrimSpace(s[i:end])
			if token == "" {
				i = end
				continue
			}
			encoded, err := json.Marshal(token)
			if err != nil {
				return "", err
			}
			out.Write(encoded)
			i = end
		}
	}
	if len(depthStack) != 0 {
		return "", fmt.Errorf("unbalanced flow collection %q", s)
	}
	return out.String(), nil
}

func isFlowStructural(c byte) bool {
	switch c {
	case ',', ':', '{', '}', '[', ']', '"', '\'':
		return true
	}
	return false
}

// unquote removes matching surrounding quotes and processes the most common
// escape sequences. Unquoted input is returned unchanged.
func unquote(s string) string {
	if len(s) < 2 {
		return s
	}
	if s[0] == '"' && s[len(s)-1] == '"' {
		// Use strconv for proper JSON-style unescaping.
		if unquoted, err := strconv.Unquote(s); err == nil {
			return unquoted
		}
	}
	if s[0] == '\'' && s[len(s)-1] == '\'' {
		// YAML single quotes: only '' escapes a literal single quote.
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	return s
}
