package tinyxml

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Header is the standard XML 1.0 UTF-8 declaration prefix.
const Header = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"

const (
	maxNestingDepth = 64
	maxTotalNodes   = 100_000
)

// Attr represents a parsed XML attribute with optional namespace Prefix and Local name.
type Attr struct {
	Prefix string
	Local  string
	Value  string
}

// Node represents a parsed XML element node.
type Node struct {
	Prefix   string
	Local    string
	Attrs    []Attr
	Text     string
	Children []*Node
}

// Attr returns the value of the first attribute matching localCaseInsensitive (or "prefix:local").
func (n *Node) Attr(localCaseInsensitive string) string {
	if n == nil || localCaseInsensitive == "" {
		return ""
	}
	wantPfx, wantLocal, hasPfx := strings.Cut(localCaseInsensitive, ":")
	for _, a := range n.Attrs {
		if hasPfx {
			if strings.EqualFold(a.Prefix, wantPfx) && strings.EqualFold(a.Local, wantLocal) {
				return a.Value
			}
		} else if strings.EqualFold(a.Local, localCaseInsensitive) {
			return a.Value
		}
	}
	return ""
}

// AttrExact returns the value of the first attribute matching local case-sensitively (or "prefix:local").
func (n *Node) AttrExact(local string) string {
	if n == nil || local == "" {
		return ""
	}
	wantPfx, wantLocal, hasPfx := strings.Cut(local, ":")
	for _, a := range n.Attrs {
		if hasPfx {
			if a.Prefix == wantPfx && a.Local == wantLocal {
				return a.Value
			}
		} else if a.Local == local {
			return a.Value
		}
	}
	return ""
}

// Child returns the first child element matching local (preferring exact case match, with case-insensitive fallback).
func (n *Node) Child(local string) *Node {
	if n == nil || local == "" {
		return nil
	}
	wantPfx, wantLocal, hasPfx := strings.Cut(local, ":")
	for _, c := range n.Children {
		if hasPfx {
			if c.Prefix == wantPfx && c.Local == wantLocal {
				return c
			}
		} else if c.Local == local {
			return c
		}
	}
	for _, c := range n.Children {
		if hasPfx {
			if strings.EqualFold(c.Prefix, wantPfx) && strings.EqualFold(c.Local, wantLocal) {
				return c
			}
		} else if strings.EqualFold(c.Local, local) {
			return c
		}
	}
	return nil
}

// ChildText returns the decoded text content of the first matching child element, or "" if absent.
func (n *Node) ChildText(local string) string {
	if c := n.Child(local); c != nil {
		return c.Text
	}
	return ""
}

// ChildrenByLocal returns all direct child elements matching local (exact match first, case-insensitive fallback).
func (n *Node) ChildrenByLocal(local string) []*Node {
	if n == nil || local == "" || len(n.Children) == 0 {
		return nil
	}
	wantPfx, wantLocal, hasPfx := strings.Cut(local, ":")
	var out []*Node
	for _, c := range n.Children {
		if hasPfx {
			if c.Prefix == wantPfx && c.Local == wantLocal {
				out = append(out, c)
			}
		} else if c.Local == local {
			out = append(out, c)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, c := range n.Children {
		if hasPfx {
			if strings.EqualFold(c.Prefix, wantPfx) && strings.EqualFold(c.Local, wantLocal) {
				out = append(out, c)
			}
		} else if strings.EqualFold(c.Local, local) {
			out = append(out, c)
		}
	}
	return out
}

// EscapeText escapes XML special characters (&, <, >, ", ') and strips invalid XML 1.0 control characters.
func EscapeText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, width := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && width == 1 {
			b.WriteRune('\uFFFD')
			i++
			continue
		}
		i += width
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			if isValidXMLChar(r) {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

func isValidXMLChar(r rune) bool {
	return r == 0x09 || r == 0x0A || r == 0x0D ||
		(r >= 0x20 && r <= 0xD7FF) ||
		(r >= 0xE000 && r <= 0xFFFD) ||
		(r >= 0x10000 && r <= 0x10FFFF)
}

// Parse parses a complete XML document into a zero-reflection Node tree with strict depth and node-count caps.
func Parse(data []byte) (*Node, error) {
	// Strip UTF-8 BOM if present.
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	n := len(data)
	pos := 0

	var (
		root      *Node
		stack     []*Node
		textBufs  [][]byte
		nodeCount int
	)

	for pos < n {
		lt := bytes.IndexByte(data[pos:], '<')
		if lt < 0 {
			if len(stack) > 0 {
				return nil, fmt.Errorf("tinyxml: unclosed element <%s>", formatQName(stack[len(stack)-1].Prefix, stack[len(stack)-1].Local))
			}
			if len(bytes.TrimSpace(data[pos:])) > 0 {
				return nil, errors.New("tinyxml: unexpected character data outside root element")
			}
			break
		}
		if lt > 0 {
			chunk := data[pos : pos+lt]
			if len(stack) == 0 {
				if len(bytes.TrimSpace(chunk)) > 0 {
					return nil, errors.New("tinyxml: unexpected character data outside root element")
				}
			} else {
				dec, err := decodeEntities(string(chunk))
				if err != nil {
					return nil, err
				}
				idx := len(textBufs) - 1
				textBufs[idx] = append(textBufs[idx], dec...)
			}
			pos += lt
		}

		if pos+1 >= n {
			return nil, errors.New("tinyxml: truncated '<' at end of input")
		}

		// Processing instruction / XML declaration: <?...?>
		if data[pos+1] == '?' {
			end := bytes.Index(data[pos+2:], []byte("?>"))
			if end < 0 {
				return nil, errors.New("tinyxml: unclosed processing instruction")
			}
			pos += 2 + end + 2
			continue
		}

		// Comment, CDATA, or DOCTYPE: <!...>
		if data[pos+1] == '!' {
			if bytes.HasPrefix(data[pos:], []byte("<!--")) {
				end := bytes.Index(data[pos+4:], []byte("-->"))
				if end < 0 {
					return nil, errors.New("tinyxml: unclosed XML comment")
				}
				pos += 4 + end + 3
				continue
			}
			if bytes.HasPrefix(data[pos:], []byte("<![CDATA[")) {
				if len(stack) == 0 {
					return nil, errors.New("tinyxml: CDATA section outside root element")
				}
				end := bytes.Index(data[pos+9:], []byte("]]>"))
				if end < 0 {
					return nil, errors.New("tinyxml: unclosed CDATA section")
				}
				idx := len(textBufs) - 1
				textBufs[idx] = append(textBufs[idx], data[pos+9:pos+9+end]...)
				pos += 9 + end + 3
				continue
			}
			if len(data[pos:]) >= 9 && strings.EqualFold(string(data[pos:pos+9]), "<!DOCTYPE") {
				nextPos, err := skipDoctype(data, pos)
				if err != nil {
					return nil, err
				}
				pos = nextPos
				continue
			}
			return nil, errors.New("tinyxml: unsupported or malformed '<!' construct")
		}

		// Closing tag: </name>
		if data[pos+1] == '/' {
			pos += 2
			nameEnd := scanName(data, pos)
			if nameEnd == pos {
				return nil, errors.New("tinyxml: empty closing tag name")
			}
			pfx, loc, err := splitQName(string(data[pos:nameEnd]))
			if err != nil {
				return nil, err
			}
			pos = skipXMLSpace(data, nameEnd)
			if pos >= n || data[pos] != '>' {
				return nil, fmt.Errorf("tinyxml: malformed closing tag </%s>", formatQName(pfx, loc))
			}
			pos++
			if len(stack) == 0 {
				return nil, fmt.Errorf("tinyxml: unexpected closing tag </%s>", formatQName(pfx, loc))
			}
			top := stack[len(stack)-1]
			if top.Prefix != pfx || top.Local != loc {
				return nil, fmt.Errorf("tinyxml: mismatched closing tag </%s> for <%s>", formatQName(pfx, loc), formatQName(top.Prefix, top.Local))
			}
			if b := textBufs[len(textBufs)-1]; len(b) > 0 {
				top.Text = string(b)
			}
			stack = stack[:len(stack)-1]
			textBufs = textBufs[:len(textBufs)-1]
			if len(stack) == 0 {
				root = top
			}
			continue
		}

		// Opening or self-closing tag: <name ...>
		if root != nil && len(stack) == 0 {
			return nil, errors.New("tinyxml: multiple root elements")
		}
		if len(stack) >= maxNestingDepth {
			return nil, fmt.Errorf("tinyxml: maximum nesting depth (%d) exceeded", maxNestingDepth)
		}
		nodeCount++
		if nodeCount > maxTotalNodes {
			return nil, fmt.Errorf("tinyxml: maximum node count (%d) exceeded", maxTotalNodes)
		}

		pos++ // skip '<'
		nameEnd := scanName(data, pos)
		if nameEnd == pos {
			return nil, errors.New("tinyxml: malformed opening tag name")
		}
		pfx, loc, err := splitQName(string(data[pos:nameEnd]))
		if err != nil {
			return nil, err
		}
		elem := &Node{Prefix: pfx, Local: loc}
		pos = nameEnd

		selfClosing := false
		for {
			prevPos := pos
			pos = skipXMLSpace(data, pos)
			if pos >= n {
				return nil, fmt.Errorf("tinyxml: unclosed start tag <%s>", formatQName(pfx, loc))
			}
			if data[pos] == '>' {
				pos++
				break
			}
			if data[pos] == '/' {
				if pos+1 >= n || data[pos+1] != '>' {
					return nil, fmt.Errorf("tinyxml: expected '/>' in tag <%s>", formatQName(pfx, loc))
				}
				selfClosing = true
				pos += 2
				break
			}
			if pos == prevPos {
				return nil, fmt.Errorf("tinyxml: missing whitespace before attribute in <%s>", formatQName(pfx, loc))
			}
			attrEnd := scanName(data, pos)
			if attrEnd == pos {
				return nil, fmt.Errorf("tinyxml: invalid attribute name in <%s>", formatQName(pfx, loc))
			}
			aPfx, aLoc, aErr := splitQName(string(data[pos:attrEnd]))
			if aErr != nil {
				return nil, aErr
			}
			pos = skipXMLSpace(data, attrEnd)
			if pos >= n || data[pos] != '=' {
				return nil, fmt.Errorf("tinyxml: missing '=' for attribute %q in <%s>", formatQName(aPfx, aLoc), formatQName(pfx, loc))
			}
			pos = skipXMLSpace(data, pos+1)
			if pos >= n || (data[pos] != '"' && data[pos] != '\'') {
				return nil, fmt.Errorf("tinyxml: unquoted attribute %q in <%s>", formatQName(aPfx, aLoc), formatQName(pfx, loc))
			}
			quote := data[pos]
			pos++
			qEnd := bytes.IndexByte(data[pos:], quote)
			if qEnd < 0 {
				return nil, fmt.Errorf("tinyxml: unclosed quote in attribute %q", formatQName(aPfx, aLoc))
			}
			rawVal := data[pos : pos+qEnd]
			if bytes.IndexByte(rawVal, '<') >= 0 {
				return nil, fmt.Errorf("tinyxml: unescaped '<' in attribute %q", formatQName(aPfx, aLoc))
			}
			decVal, dErr := decodeEntities(string(rawVal))
			if dErr != nil {
				return nil, dErr
			}
			elem.Attrs = append(elem.Attrs, Attr{Prefix: aPfx, Local: aLoc, Value: decVal})
			pos += qEnd + 1
		}

		if len(stack) > 0 {
			parent := stack[len(stack)-1]
			parent.Children = append(parent.Children, elem)
		}
		if selfClosing {
			if len(stack) == 0 {
				root = elem
			}
		} else {
			stack = append(stack, elem)
			textBufs = append(textBufs, nil)
		}
	}

	if len(stack) > 0 {
		return nil, fmt.Errorf("tinyxml: unclosed element <%s>", formatQName(stack[len(stack)-1].Prefix, stack[len(stack)-1].Local))
	}
	if root == nil {
		return nil, errors.New("tinyxml: document contains no root element")
	}
	return root, nil
}

func skipDoctype(data []byte, start int) (int, error) {
	i := start + 9
	n := len(data)
	bracketDepth := 0
	for i < n {
		c := data[i]
		if c == '"' || c == '\'' {
			i++
			end := bytes.IndexByte(data[i:], c)
			if end < 0 {
				return 0, errors.New("tinyxml: unclosed quote in DOCTYPE")
			}
			i += end + 1
			continue
		}
		if c == '<' && bytes.HasPrefix(data[i:], []byte("<!--")) {
			end := bytes.Index(data[i+4:], []byte("-->"))
			if end < 0 {
				return 0, errors.New("tinyxml: unclosed comment in DOCTYPE")
			}
			i += 4 + end + 3
			continue
		}
		if c == '[' {
			bracketDepth++
		} else if c == ']' {
			if bracketDepth == 0 {
				return 0, errors.New("tinyxml: unexpected ']' in DOCTYPE")
			}
			bracketDepth--
		} else if c == '>' && bracketDepth == 0 {
			return i + 1, nil
		}
		i++
	}
	return 0, errors.New("tinyxml: unclosed DOCTYPE declaration")
}

func skipXMLSpace(data []byte, i int) int {
	for i < len(data) {
		switch data[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

func scanName(data []byte, start int) int {
	i := start
	for i < len(data) {
		r, w := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && w == 1 {
			break
		}
		if i == start {
			if !isNameStartRune(r) {
				break
			}
		} else if !isNameRune(r) {
			break
		}
		i += w
	}
	return i
}

func isNameStartRune(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_' || r == ':' ||
		(r >= 0xC0 && r != 0xD7 && r != 0xF7 && r <= 0x10FFFF)
}

func isNameRune(r rune) bool {
	return isNameStartRune(r) || (r >= '0' && r <= '9') || r == '-' || r == '.' || r == 0xB7
}

func splitQName(q string) (string, string, error) {
	pfx, loc, hasColon := strings.Cut(q, ":")
	if !hasColon {
		return "", q, nil
	}
	if pfx == "" || loc == "" || strings.Contains(loc, ":") {
		return "", "", fmt.Errorf("tinyxml: invalid qualified name %q", q)
	}
	return pfx, loc, nil
}

func formatQName(pfx, loc string) string {
	if pfx != "" {
		return pfx + ":" + loc
	}
	return loc
}

func decodeEntities(s string) (string, error) {
	if strings.IndexByte(s, '&') < 0 {
		return s, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		amp := strings.IndexByte(s[i:], '&')
		if amp < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+amp])
		i += amp
		semi := strings.IndexByte(s[i:], ';')
		if semi <= 1 {
			return "", errors.New("tinyxml: malformed or unclosed entity reference")
		}
		ent := s[i+1 : i+semi]
		i += semi + 1
		switch ent {
		case "amp":
			b.WriteByte('&')
		case "lt":
			b.WriteByte('<')
		case "gt":
			b.WriteByte('>')
		case "quot":
			b.WriteByte('"')
		case "apos":
			b.WriteByte('\'')
		default:
			if ent[0] != '#' || len(ent) < 2 {
				return "", fmt.Errorf("tinyxml: unknown XML entity &%s;", ent)
			}
			var code uint64
			var err error
			if ent[1] == 'x' || ent[1] == 'X' {
				code, err = strconv.ParseUint(ent[2:], 16, 32)
			} else {
				code, err = strconv.ParseUint(ent[1:], 10, 32)
			}
			if err != nil || code == 0 || code > utf8.MaxRune || !utf8.ValidRune(rune(code)) || !isValidXMLChar(rune(code)) {
				return "", fmt.Errorf("tinyxml: invalid character reference &%s;", ent)
			}
			b.WriteRune(rune(code))
		}
	}
	return b.String(), nil
}
