package agentsessions

import (
	"bytes"
	"encoding/json"
	"errors"
)

var pathKeys = map[string]bool{
	"cwd": true, "working_directory": true, "workdir": true,
	"workspace": true, "workspace_path": true, "project_path": true,
	"projectPath": true, "path": true, "file_path": true, "filePath": true,
	"directory": true, "root": true, "writable_roots": true, "readable_roots": true,
	"runtime_workspace_roots": true, "live_cwd": true, "rule_roots": true, "resolved": true,
}

// These are opaque user/model/tool content, even if they contain structured JSON
// with a property called "path". Only adapter metadata is eligible for remapping.
var contentKeys = map[string]bool{
	"message": true, "content": true, "text": true, "input": true, "output": true,
	"arguments": true, "encrypted_content": true, "tool_input": true, "tool_result": true,
}

func jsonValidObject(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed)
}

// rewrite preserves every original byte except the string tokens for remapped
// metadata paths. Whitespace, key order, number spelling and opaque JSON stay
// intact, including CRLFs and the absence of a final newline.
func rewrite(data []byte, source, target string) ([]byte, error) {
	return rewriteAliases(data, source, target, nil)
}

func rewriteAliases(data []byte, source, target string, aliases map[string]string) ([]byte, error) {
	var out bytes.Buffer
	offset := 0
	for offset < len(data) {
		next := bytes.IndexByte(data[offset:], '\n')
		end := len(data)
		if next >= 0 {
			end = offset + next
		}
		line := data[offset:end]
		if len(bytes.TrimSpace(line)) == 0 {
			out.Write(line)
		} else {
			if !jsonValidObject(line) {
				return nil, errors.New("native JSONL contains malformed or non-object content")
			}
			parser := pathRewriter{data: line, transform: func(value string) string { return remap(aliasPath(value, aliases), source, target) }}
			parser.value(false, false)
			parser.output.Write(line[parser.copied:])
			out.Write(parser.output.Bytes())
		}
		if next < 0 {
			break
		}
		out.WriteByte('\n')
		offset = end + 1
	}
	return out.Bytes(), nil
}

type pathRewriter struct {
	data        []byte
	pos, copied int
	transform   func(string) string
	output      bytes.Buffer
}

func (p *pathRewriter) space() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\r', '\n':
			p.pos++
		default:
			return
		}
	}
}
func (p *pathRewriter) stringToken() (int, int, string) {
	start := p.pos
	p.pos++
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		p.pos++
		if c == '\\' {
			p.pos++
			continue
		}
		if c == '"' {
			break
		}
	}
	var decoded string
	_ = json.Unmarshal(p.data[start:p.pos], &decoded)
	return start, p.pos, decoded
}
func (p *pathRewriter) value(pathValue, opaque bool) {
	p.space()
	switch p.data[p.pos] {
	case '{':
		p.pos++
		p.space()
		if p.data[p.pos] == '}' {
			p.pos++
			return
		}
		for {
			p.space()
			_, _, key := p.stringToken()
			p.space()
			p.pos++
			p.value(pathValue || pathKeys[key], opaque || contentKeys[key])
			p.space()
			delim := p.data[p.pos]
			p.pos++
			if delim == '}' {
				break
			}
		}
	case '[':
		p.pos++
		p.space()
		if p.data[p.pos] == ']' {
			p.pos++
			return
		}
		for {
			p.value(pathValue, opaque)
			p.space()
			delim := p.data[p.pos]
			p.pos++
			if delim == ']' {
				break
			}
		}
	case '"':
		start, end, value := p.stringToken()
		if pathValue && !opaque {
			mapped := p.transform(value)
			if mapped != value {
				p.output.Write(p.data[p.copied:start])
				var encoded bytes.Buffer
				encoder := json.NewEncoder(&encoded)
				encoder.SetEscapeHTML(false)
				_ = encoder.Encode(mapped)
				p.output.Write(bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'}))
				p.copied = end
			}
		}
	default:
		for p.pos < len(p.data) {
			switch p.data[p.pos] {
			case ',', ']', '}', ' ', '\t', '\n', '\r':
				return
			default:
				p.pos++
			}
		}
	}
}
