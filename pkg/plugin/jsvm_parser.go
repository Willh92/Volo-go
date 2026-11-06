package plugin

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

type jsTokKind int

const (
	tokEOF jsTokKind = iota
	tokIdent
	tokNumber
	tokString
	tokRegex
	tokTemplate // raw template literal without interpolation or desugared
	tokLParen
	tokRParen
	tokLBrace
	tokRBrace
	tokLBrack
	tokRBrack
	tokSemi
	tokComma
	tokDot
	tokOptChain // ?.
	tokColon
	tokQuestion
	tokArrow // =>
	tokAssign
	tokPlusAssign
	tokMinusAssign
	tokStarAssign
	tokSlashAssign
	tokPctAssign
	tokPlus
	tokMinus
	tokStar
	tokSlash
	tokPercent
	tokInc
	tokDec
	tokEq
	tokNeq
	tokStrictEq
	tokStrictNeq
	tokLt
	tokLte
	tokGt
	tokGte
	tokAnd
	tokOr
	tokNullish
	tokNot
	tokBitAnd
	tokBitOr
	tokBitXor
	tokBitNot
	tokShl
	tokShr
	tokUShr

	// Keywords
	tokVar
	tokLet
	tokConst
	tokFunc
	tokReturn
	tokIf
	tokElse
	tokWhile
	tokDo
	tokFor
	tokIn
	tokOf
	tokBreak
	tokContinue
	tokSwitch
	tokCase
	tokDefault
	tokTry
	tokCatch
	tokFinally
	tokThrow
	tokNew
	tokTypeof
	tokInstanceof
	tokVoid
	tokDelete
	tokTrue
	tokFalse
	tokNull
	tokUndefined
	tokThis
)

var jsKeywords = map[string]jsTokKind{
	"var":        tokVar,
	"let":        tokLet,
	"const":      tokConst,
	"function":   tokFunc,
	"return":     tokReturn,
	"if":         tokIf,
	"else":       tokElse,
	"while":      tokWhile,
	"do":         tokDo,
	"for":        tokFor,
	"in":         tokIn,
	"of":         tokOf,
	"break":      tokBreak,
	"continue":   tokContinue,
	"switch":     tokSwitch,
	"case":       tokCase,
	"default":    tokDefault,
	"try":        tokTry,
	"catch":      tokCatch,
	"finally":    tokFinally,
	"throw":      tokThrow,
	"new":        tokNew,
	"typeof":     tokTypeof,
	"instanceof": tokInstanceof,
	"void":       tokVoid,
	"delete":     tokDelete,
	"true":       tokTrue,
	"false":      tokFalse,
	"null":       tokNull,
	"undefined":  tokUndefined,
	"this":       tokThis,
}

type jsToken struct {
	kind    jsTokKind
	lit     string
	aux     string // regex flags
	num     float64
	line    int
	newline bool // preceded by a line terminator
}

func tokenizeJS(src string) ([]jsToken, error) {
	var toks []jsToken
	n := len(src)
	i := 0
	line := 1
	sawNL := false

	canPrecedeRegex := func() bool {
		if len(toks) == 0 {
			return true
		}
		prev := toks[len(toks)-1].kind
		switch prev {
		case tokIdent, tokNumber, tokString, tokRegex, tokTemplate,
			tokRParen, tokRBrack, tokRBrace, tokInc, tokDec,
			tokTrue, tokFalse, tokNull, tokUndefined, tokThis:
			return false
		default:
			return true
		}
	}

	emit := func(k jsTokKind, lit string) {
		toks = append(toks, jsToken{kind: k, lit: lit, line: line, newline: sawNL})
		sawNL = false
	}

	for i < n {
		ch := src[i]
		if ch == ' ' || ch == '\t' || ch == '\r' {
			i++
			continue
		}
		if ch == '\n' {
			line++
			sawNL = true
			i++
			continue
		}
		// Comments
		if ch == '/' && i+1 < n && src[i+1] == '/' {
			i += 2
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '/' && i+1 < n && src[i+1] == '*' {
			i += 2
			closed := false
			for i+1 < n {
				if src[i] == '\n' {
					line++
					sawNL = true
				}
				if src[i] == '*' && src[i+1] == '/' {
					i += 2
					closed = true
					break
				}
				i++
			}
			if !closed {
				return nil, fmt.Errorf("line %d: unterminated block comment", line)
			}
			continue
		}

		// String literals
		if ch == '"' || ch == '\'' {
			quote := ch
			i++
			var sb strings.Builder
			closed := false
			for i < n {
				c := src[i]
				if c == '\\' {
					i++
					if i >= n {
						break
					}
					esc := src[i]
					i++
					switch esc {
					case 'n':
						sb.WriteByte('\n')
					case 'r':
						sb.WriteByte('\r')
					case 't':
						sb.WriteByte('\t')
					case '\\', '\'', '"', '/':
						sb.WriteByte(esc)
					case 'x':
						if i+2 <= n {
							if v, err := strconv.ParseUint(src[i:i+2], 16, 8); err == nil {
								sb.WriteByte(byte(v))
								i += 2
								continue
							}
						}
						sb.WriteByte('x')
					case 'u':
						if i+4 <= n {
							if v, err := strconv.ParseUint(src[i:i+4], 16, 16); err == nil {
								sb.WriteRune(rune(v))
								i += 4
								continue
							}
						}
						sb.WriteByte('u')
					default:
						sb.WriteByte(esc)
					}
					continue
				}
				if c == quote {
					i++
					closed = true
					break
				}
				if c == '\n' {
					return nil, fmt.Errorf("line %d: unterminated string literal", line)
				}
				sb.WriteByte(c)
				i++
			}
			if !closed {
				return nil, fmt.Errorf("line %d: unterminated string literal", line)
			}
			emit(tokString, sb.String())
			continue
		}

		// Template literal `...${expr}...` desugared into ("a" + String(expr) + "b")
		if ch == '`' {
			i++
			emit(tokLParen, "(")
			var sb strings.Builder
			firstPart := true
			flushStr := func() {
				if !firstPart {
					emit(tokPlus, "+")
				}
				emit(tokString, sb.String())
				sb.Reset()
				firstPart = false
			}
			closed := false
			for i < n {
				c := src[i]
				if c == '\\' && i+1 < n {
					i++
					switch src[i] {
					case 'n':
						sb.WriteByte('\n')
					case 'r':
						sb.WriteByte('\r')
					case 't':
						sb.WriteByte('\t')
					default:
						sb.WriteByte(src[i])
					}
					i++
					continue
				}
				if c == '$' && i+1 < n && src[i+1] == '{' {
					flushStr()
					i += 2
					depth := 1
					exprStart := i
					for i < n && depth > 0 {
						if src[i] == '{' {
							depth++
						} else if src[i] == '}' {
							depth--
							if depth == 0 {
								break
							}
						} else if src[i] == '\n' {
							line++
						}
						i++
					}
					if depth != 0 {
						return nil, fmt.Errorf("line %d: unterminated template interpolation", line)
					}
					subToks, err := tokenizeJS(src[exprStart:i])
					if err != nil {
						return nil, err
					}
					i++ // skip '}'
					emit(tokPlus, "+")
					emit(tokIdent, "String")
					emit(tokLParen, "(")
					for _, st := range subToks {
						if st.kind != tokEOF {
							toks = append(toks, st)
						}
					}
					emit(tokRParen, ")")
					continue
				}
				if c == '`' {
					i++
					closed = true
					break
				}
				if c == '\n' {
					line++
				}
				sb.WriteByte(c)
				i++
			}
			if !closed {
				return nil, fmt.Errorf("line %d: unterminated template literal", line)
			}
			flushStr()
			emit(tokRParen, ")")
			continue
		}

		// Regex literal /pattern/flags
		if ch == '/' && canPrecedeRegex() {
			i++
			var pat strings.Builder
			inClass := false
			closed := false
			for i < n {
				c := src[i]
				if c == '\\' && i+1 < n {
					pat.WriteByte(c)
					pat.WriteByte(src[i+1])
					i += 2
					continue
				}
				if c == '[' {
					inClass = true
				} else if c == ']' {
					inClass = false
				} else if c == '/' && !inClass {
					i++
					closed = true
					break
				} else if c == '\n' {
					return nil, fmt.Errorf("line %d: unterminated regex literal", line)
				}
				pat.WriteByte(c)
				i++
			}
			if !closed {
				return nil, fmt.Errorf("line %d: unterminated regex literal", line)
			}
			var flags strings.Builder
			for i < n && (src[i] >= 'a' && src[i] <= 'z') {
				flags.WriteByte(src[i])
				i++
			}
			toks = append(toks, jsToken{
				kind:    tokRegex,
				lit:     pat.String(),
				aux:     flags.String(),
				line:    line,
				newline: sawNL,
			})
			sawNL = false
			continue
		}

		// Numbers
		if (ch >= '0' && ch <= '9') || (ch == '.' && i+1 < n && src[i+1] >= '0' && src[i+1] <= '9') {
			start := i
			if ch == '0' && i+1 < n && (src[i+1] == 'x' || src[i+1] == 'X') {
				i += 2
				for i < n && ((src[i] >= '0' && src[i] <= '9') || (src[i] >= 'a' && src[i] <= 'f') || (src[i] >= 'A' && src[i] <= 'F')) {
					i++
				}
				v, err := strconv.ParseUint(src[start+2:i], 16, 64)
				if err != nil {
					return nil, fmt.Errorf("line %d: invalid hex literal", line)
				}
				toks = append(toks, jsToken{kind: tokNumber, lit: src[start:i], num: float64(v), line: line, newline: sawNL})
				sawNL = false
				continue
			}
			for i < n && src[i] >= '0' && src[i] <= '9' {
				i++
			}
			if i < n && src[i] == '.' {
				i++
				for i < n && src[i] >= '0' && src[i] <= '9' {
					i++
				}
			}
			if i < n && (src[i] == 'e' || src[i] == 'E') {
				i++
				if i < n && (src[i] == '+' || src[i] == '-') {
					i++
				}
				for i < n && src[i] >= '0' && src[i] <= '9' {
					i++
				}
			}
			v, err := strconv.ParseFloat(src[start:i], 64)
			if err != nil {
				return nil, fmt.Errorf("line %d: invalid number %q", line, src[start:i])
			}
			toks = append(toks, jsToken{kind: tokNumber, lit: src[start:i], num: v, line: line, newline: sawNL})
			sawNL = false
			continue
		}

		// Identifiers & keywords
		if ch == '$' || ch == '_' || unicode.IsLetter(rune(ch)) {
			start := i
			for i < n {
				r := rune(src[i])
				if r == '$' || r == '_' || (r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r))) || r >= 128 {
					i++
				} else {
					break
				}
			}
			word := src[start:i]
			if kw, ok := jsKeywords[word]; ok {
				emit(kw, word)
			} else {
				emit(tokIdent, word)
			}
			continue
		}

		// Multi-char operators
		rem := src[i:]
		switch {
		case strings.HasPrefix(rem, "==="):
			emit(tokStrictEq, "===")
			i += 3
		case strings.HasPrefix(rem, "!=="):
			emit(tokStrictNeq, "!==")
			i += 3
		case strings.HasPrefix(rem, ">>>"):
			emit(tokUShr, ">>>")
			i += 3
		case strings.HasPrefix(rem, "=>"):
			emit(tokArrow, "=>")
			i += 2
		case strings.HasPrefix(rem, "?."):
			emit(tokOptChain, "?.")
			i += 2
		case strings.HasPrefix(rem, "??"):
			emit(tokNullish, "??")
			i += 2
		case strings.HasPrefix(rem, "=="):
			emit(tokEq, "==")
			i += 2
		case strings.HasPrefix(rem, "!="):
			emit(tokNeq, "!=")
			i += 2
		case strings.HasPrefix(rem, "<="):
			emit(tokLte, "<=")
			i += 2
		case strings.HasPrefix(rem, ">="):
			emit(tokGte, ">=")
			i += 2
		case strings.HasPrefix(rem, "&&"):
			emit(tokAnd, "&&")
			i += 2
		case strings.HasPrefix(rem, "||"):
			emit(tokOr, "||")
			i += 2
		case strings.HasPrefix(rem, "++"):
			emit(tokInc, "++")
			i += 2
		case strings.HasPrefix(rem, "--"):
			emit(tokDec, "--")
			i += 2
		case strings.HasPrefix(rem, "+="):
			emit(tokPlusAssign, "+=")
			i += 2
		case strings.HasPrefix(rem, "-="):
			emit(tokMinusAssign, "-=")
			i += 2
		case strings.HasPrefix(rem, "*="):
			emit(tokStarAssign, "*=")
			i += 2
		case strings.HasPrefix(rem, "/="):
			emit(tokSlashAssign, "/=")
			i += 2
		case strings.HasPrefix(rem, "%="):
			emit(tokPctAssign, "%=")
			i += 2
		case strings.HasPrefix(rem, "<<"):
			emit(tokShl, "<<")
			i += 2
		case strings.HasPrefix(rem, ">>"):
			emit(tokShr, ">>")
			i += 2
		default:
			i++
			switch ch {
			case '(':
				emit(tokLParen, "(")
			case ')':
				emit(tokRParen, ")")
			case '{':
				emit(tokLBrace, "{")
			case '}':
				emit(tokRBrace, "}")
			case '[':
				emit(tokLBrack, "[")
			case ']':
				emit(tokRBrack, "]")
			case ';':
				emit(tokSemi, ";")
			case ',':
				emit(tokComma, ",")
			case '.':
				emit(tokDot, ".")
			case ':':
				emit(tokColon, ":")
			case '?':
				emit(tokQuestion, "?")
			case '=':
				emit(tokAssign, "=")
			case '+':
				emit(tokPlus, "+")
			case '-':
				emit(tokMinus, "-")
			case '*':
				emit(tokStar, "*")
			case '/':
				emit(tokSlash, "/")
			case '%':
				emit(tokPercent, "%")
			case '!':
				emit(tokNot, "!")
			case '<':
				emit(tokLt, "<")
			case '>':
				emit(tokGt, ">")
			case '&':
				emit(tokBitAnd, "&")
			case '|':
				emit(tokBitOr, "|")
			case '^':
				emit(tokBitXor, "^")
			case '~':
				emit(tokBitNot, "~")
			default:
				return nil, fmt.Errorf("line %d: unexpected character %q", line, ch)
			}
		}
	}
	toks = append(toks, jsToken{kind: tokEOF, line: line, newline: sawNL})
	return toks, nil
}

// --- AST Definitions ---

type jsProgram struct {
	name  string
	stmts []jsStmt
}

type jsStmt interface{ stmtNode() }
type jsExpr interface{ exprNode() }

type stmtBlock struct{ stmts []jsStmt }
type stmtVarDecl struct {
	kind  jsTokKind
	decls []varDeclarator
}
type varDeclarator struct {
	name string
	init jsExpr
}
type stmtFuncDecl struct {
	name   string
	params []string
	body   []jsStmt
}
type stmtReturn struct{ value jsExpr }
type stmtIf struct {
	cond jsExpr
	then jsStmt
	alt  jsStmt
}
type stmtWhile struct {
	cond jsExpr
	body jsStmt
}
type stmtDoWhile struct {
	body jsStmt
	cond jsExpr
}
type stmtFor struct {
	init jsStmt
	cond jsExpr
	post jsExpr
	body jsStmt
}
type stmtForInOf struct {
	isOf    bool
	varName string
	right   jsExpr
	body    jsStmt
}
type stmtSwitch struct {
	disc  jsExpr
	cases []switchClause
}
type switchClause struct {
	test  jsExpr // nil = default
	stmts []jsStmt
}
type stmtBreak struct{}
type stmtContinue struct{}
type stmtThrow struct{ value jsExpr }
type stmtTry struct {
	block     []jsStmt
	catchName string
	hasCatch  bool
	handler   []jsStmt
	finalizer []jsStmt
}
type stmtExpr struct{ expr jsExpr }
type stmtEmpty struct{}

func (*stmtBlock) stmtNode()    {}
func (*stmtVarDecl) stmtNode()  {}
func (*stmtFuncDecl) stmtNode() {}
func (*stmtReturn) stmtNode()   {}
func (*stmtIf) stmtNode()       {}
func (*stmtWhile) stmtNode()    {}
func (*stmtDoWhile) stmtNode()  {}
func (*stmtFor) stmtNode()      {}
func (*stmtForInOf) stmtNode()  {}
func (*stmtSwitch) stmtNode()   {}
func (*stmtBreak) stmtNode()    {}
func (*stmtContinue) stmtNode() {}
func (*stmtThrow) stmtNode()    {}
func (*stmtTry) stmtNode()      {}
func (*stmtExpr) stmtNode()     {}
func (*stmtEmpty) stmtNode()    {}

type exprIdent struct{ name string }
type exprNumber struct{ val float64 }
type exprString struct{ val string }
type exprBool struct{ val bool }
type exprNull struct{}
type exprUndefined struct{}
type exprThis struct{}
type exprRegex struct{ pattern, flags string }
type exprArray struct{ elems []jsExpr }
type exprObject struct{ props []objProp }
type objProp struct {
	key string
	val jsExpr
}
type exprFunc struct {
	name   string
	params []string
	body   []jsStmt
	arrow  bool
}
type exprUnary struct {
	op  jsTokKind
	arg jsExpr
}
type exprUpdate struct {
	op     jsTokKind
	prefix bool
	arg    jsExpr
}
type exprBinary struct {
	op          jsTokKind
	left, right jsExpr
}
type exprAssign struct {
	op          jsTokKind
	left, right jsExpr
}
type exprCond struct {
	test, cons, alt jsExpr
}
type exprMember struct {
	obj      jsExpr
	prop     string
	index    jsExpr
	computed bool
	optional bool
}
type exprCall struct {
	callee jsExpr
	args   []jsExpr
	isNew  bool
}
type exprSeq struct{ exprs []jsExpr }

func (*exprIdent) exprNode()     {}
func (*exprNumber) exprNode()    {}
func (*exprString) exprNode()    {}
func (*exprBool) exprNode()      {}
func (*exprNull) exprNode()      {}
func (*exprUndefined) exprNode() {}
func (*exprThis) exprNode()      {}
func (*exprRegex) exprNode()     {}
func (*exprArray) exprNode()     {}
func (*exprObject) exprNode()    {}
func (*exprFunc) exprNode()      {}
func (*exprUnary) exprNode()     {}
func (*exprUpdate) exprNode()    {}
func (*exprBinary) exprNode()    {}
func (*exprAssign) exprNode()    {}
func (*exprCond) exprNode()      {}
func (*exprMember) exprNode()    {}
func (*exprCall) exprNode()      {}
func (*exprSeq) exprNode()       {}

// --- Recursive-Descent Parser ---

type jsParser struct {
	toks []jsToken
	pos  int
}

func compileJSProgram(name string, src string) (*jsProgram, error) {
	toks, err := tokenizeJS(src)
	if err != nil {
		return nil, err
	}
	p := &jsParser{toks: toks}
	var stmts []jsStmt
	for p.cur().kind != tokEOF {
		st, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, st)
	}
	return &jsProgram{name: name, stmts: stmts}, nil
}

func (p *jsParser) cur() jsToken {
	if p.pos >= len(p.toks) {
		return p.toks[len(p.toks)-1]
	}
	return p.toks[p.pos]
}

func (p *jsParser) peek(offset int) jsToken {
	idx := p.pos + offset
	if idx >= len(p.toks) {
		return p.toks[len(p.toks)-1]
	}
	return p.toks[idx]
}

func (p *jsParser) next() jsToken {
	t := p.cur()
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *jsParser) expect(k jsTokKind) (jsToken, error) {
	t := p.cur()
	if t.kind != k {
		return t, fmt.Errorf("line %d: unexpected token %q", t.line, t.lit)
	}
	p.next()
	return t, nil
}

func (p *jsParser) consumeSemi() {
	if p.cur().kind == tokSemi {
		p.next()
	}
}

func (p *jsParser) parseStmt() (jsStmt, error) {
	switch p.cur().kind {
	case tokSemi:
		p.next()
		return &stmtEmpty{}, nil
	case tokLBrace:
		stmts, err := p.parseBlock()
		if err != nil {
			return nil, err
		}
		return &stmtBlock{stmts: stmts}, nil
	case tokVar, tokLet, tokConst:
		st, err := p.parseVarDecl()
		if err != nil {
			return nil, err
		}
		p.consumeSemi()
		return st, nil
	case tokFunc:
		return p.parseFuncDecl()
	case tokReturn:
		p.next()
		if p.cur().kind == tokSemi || p.cur().kind == tokRBrace || p.cur().kind == tokEOF || p.cur().newline {
			p.consumeSemi()
			return &stmtReturn{}, nil
		}
		val, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		p.consumeSemi()
		return &stmtReturn{value: val}, nil
	case tokIf:
		p.next()
		if _, err := p.expect(tokLParen); err != nil {
			return nil, err
		}
		cond, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokRParen); err != nil {
			return nil, err
		}
		thenSt, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		var altSt jsStmt
		if p.cur().kind == tokElse {
			p.next()
			altSt, err = p.parseStmt()
			if err != nil {
				return nil, err
			}
		}
		return &stmtIf{cond: cond, then: thenSt, alt: altSt}, nil
	case tokWhile:
		p.next()
		if _, err := p.expect(tokLParen); err != nil {
			return nil, err
		}
		cond, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokRParen); err != nil {
			return nil, err
		}
		body, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		return &stmtWhile{cond: cond, body: body}, nil
	case tokDo:
		p.next()
		body, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokWhile); err != nil {
			return nil, err
		}
		if _, err := p.expect(tokLParen); err != nil {
			return nil, err
		}
		cond, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokRParen); err != nil {
			return nil, err
		}
		p.consumeSemi()
		return &stmtDoWhile{body: body, cond: cond}, nil
	case tokFor:
		return p.parseForStmt()
	case tokSwitch:
		return p.parseSwitchStmt()
	case tokBreak:
		p.next()
		p.consumeSemi()
		return &stmtBreak{}, nil
	case tokContinue:
		p.next()
		p.consumeSemi()
		return &stmtContinue{}, nil
	case tokThrow:
		p.next()
		val, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		p.consumeSemi()
		return &stmtThrow{value: val}, nil
	case tokTry:
		return p.parseTryStmt()
	default:
		ex, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		p.consumeSemi()
		return &stmtExpr{expr: ex}, nil
	}
}

func (p *jsParser) parseBlock() ([]jsStmt, error) {
	if _, err := p.expect(tokLBrace); err != nil {
		return nil, err
	}
	var stmts []jsStmt
	for p.cur().kind != tokRBrace && p.cur().kind != tokEOF {
		st, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, st)
	}
	if _, err := p.expect(tokRBrace); err != nil {
		return nil, err
	}
	return stmts, nil
}

func (p *jsParser) parseVarDecl() (*stmtVarDecl, error) {
	kind := p.next().kind
	var decls []varDeclarator
	for {
		id, err := p.expect(tokIdent)
		if err != nil {
			return nil, err
		}
		var initExpr jsExpr
		if p.cur().kind == tokAssign {
			p.next()
			initExpr, err = p.parseAssignExpr()
			if err != nil {
				return nil, err
			}
		}
		decls = append(decls, varDeclarator{name: id.lit, init: initExpr})
		if p.cur().kind != tokComma {
			break
		}
		p.next()
	}
	return &stmtVarDecl{kind: kind, decls: decls}, nil
}

func (p *jsParser) parseFuncDecl() (*stmtFuncDecl, error) {
	p.next() // 'function'
	id, err := p.expect(tokIdent)
	if err != nil {
		return nil, err
	}
	params, err := p.parseParamList()
	if err != nil {
		return nil, err
	}
	body, err := p.parseBlock()
	if err != nil {
		return nil, err
	}
	return &stmtFuncDecl{name: id.lit, params: params, body: body}, nil
}

func (p *jsParser) parseParamList() ([]string, error) {
	if _, err := p.expect(tokLParen); err != nil {
		return nil, err
	}
	var params []string
	for p.cur().kind != tokRParen && p.cur().kind != tokEOF {
		id, err := p.expect(tokIdent)
		if err != nil {
			return nil, err
		}
		params = append(params, id.lit)
		if p.cur().kind == tokComma {
			p.next()
		} else {
			break
		}
	}
	if _, err := p.expect(tokRParen); err != nil {
		return nil, err
	}
	return params, nil
}

func (p *jsParser) parseForStmt() (jsStmt, error) {
	p.next() // 'for'
	if _, err := p.expect(tokLParen); err != nil {
		return nil, err
	}
	// Check for `for (var/let/const x in/of expr)` or `for (x in/of expr)`
	save := p.pos
	if p.cur().kind == tokVar || p.cur().kind == tokLet || p.cur().kind == tokConst {
		p.next()
	}
	if p.cur().kind == tokIdent && (p.peek(1).kind == tokIn || p.peek(1).kind == tokOf) {
		varName := p.next().lit
		isOf := p.next().kind == tokOf
		right, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokRParen); err != nil {
			return nil, err
		}
		body, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		return &stmtForInOf{isOf: isOf, varName: varName, right: right, body: body}, nil
	}
	p.pos = save

	var initSt jsStmt
	if p.cur().kind != tokSemi {
		if p.cur().kind == tokVar || p.cur().kind == tokLet || p.cur().kind == tokConst {
			vd, err := p.parseVarDecl()
			if err != nil {
				return nil, err
			}
			initSt = vd
		} else {
			ex, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			initSt = &stmtExpr{expr: ex}
		}
	}
	if _, err := p.expect(tokSemi); err != nil {
		return nil, err
	}
	var cond jsExpr
	if p.cur().kind != tokSemi {
		var err error
		cond, err = p.parseExpr()
		if err != nil {
			return nil, err
		}
	}
	if _, err := p.expect(tokSemi); err != nil {
		return nil, err
	}
	var post jsExpr
	if p.cur().kind != tokRParen {
		var err error
		post, err = p.parseExpr()
		if err != nil {
			return nil, err
		}
	}
	if _, err := p.expect(tokRParen); err != nil {
		return nil, err
	}
	body, err := p.parseStmt()
	if err != nil {
		return nil, err
	}
	return &stmtFor{init: initSt, cond: cond, post: post, body: body}, nil
}

func (p *jsParser) parseSwitchStmt() (jsStmt, error) {
	p.next() // 'switch'
	if _, err := p.expect(tokLParen); err != nil {
		return nil, err
	}
	disc, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(tokRParen); err != nil {
		return nil, err
	}
	if _, err := p.expect(tokLBrace); err != nil {
		return nil, err
	}
	var clauses []switchClause
	for p.cur().kind != tokRBrace && p.cur().kind != tokEOF {
		var test jsExpr
		if p.cur().kind == tokCase {
			p.next()
			test, err = p.parseExpr()
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(tokColon); err != nil {
				return nil, err
			}
		} else if p.cur().kind == tokDefault {
			p.next()
			if _, err := p.expect(tokColon); err != nil {
				return nil, err
			}
		} else {
			return nil, fmt.Errorf("line %d: expected case or default in switch", p.cur().line)
		}
		var stmts []jsStmt
		for p.cur().kind != tokCase && p.cur().kind != tokDefault && p.cur().kind != tokRBrace && p.cur().kind != tokEOF {
			st, err := p.parseStmt()
			if err != nil {
				return nil, err
			}
			stmts = append(stmts, st)
		}
		clauses = append(clauses, switchClause{test: test, stmts: stmts})
	}
	if _, err := p.expect(tokRBrace); err != nil {
		return nil, err
	}
	return &stmtSwitch{disc: disc, cases: clauses}, nil
}

func (p *jsParser) parseTryStmt() (jsStmt, error) {
	p.next() // 'try'
	block, err := p.parseBlock()
	if err != nil {
		return nil, err
	}
	st := &stmtTry{block: block}
	if p.cur().kind == tokCatch {
		p.next()
		st.hasCatch = true
		if p.cur().kind == tokLParen {
			p.next()
			id, err := p.expect(tokIdent)
			if err != nil {
				return nil, err
			}
			st.catchName = id.lit
			if _, err := p.expect(tokRParen); err != nil {
				return nil, err
			}
		}
		st.handler, err = p.parseBlock()
		if err != nil {
			return nil, err
		}
	}
	if p.cur().kind == tokFinally {
		p.next()
		st.finalizer, err = p.parseBlock()
		if err != nil {
			return nil, err
		}
	}
	return st, nil
}

// --- Expression Parsing ---

func (p *jsParser) parseExpr() (jsExpr, error) {
	first, err := p.parseAssignExpr()
	if err != nil {
		return nil, err
	}
	if p.cur().kind != tokComma {
		return first, nil
	}
	exprs := []jsExpr{first}
	for p.cur().kind == tokComma {
		p.next()
		nx, err := p.parseAssignExpr()
		if err != nil {
			return nil, err
		}
		exprs = append(exprs, nx)
	}
	return &exprSeq{exprs: exprs}, nil
}

func (p *jsParser) parseAssignExpr() (jsExpr, error) {
	// Single-param arrow function `x => ...`
	if p.cur().kind == tokIdent && p.peek(1).kind == tokArrow {
		param := p.next().lit
		p.next() // '=>'
		return p.parseArrowBody([]string{param})
	}
	// Parenthesized arrow function `(a, b) => ...` or `() => ...`
	if params, ok := p.tryArrowParams(); ok {
		return p.parseArrowBody(params)
	}

	left, err := p.parseCondExpr()
	if err != nil {
		return nil, err
	}
	switch p.cur().kind {
	case tokAssign, tokPlusAssign, tokMinusAssign, tokStarAssign, tokSlashAssign, tokPctAssign:
		op := p.next().kind
		right, err := p.parseAssignExpr()
		if err != nil {
			return nil, err
		}
		return &exprAssign{op: op, left: left, right: right}, nil
	}
	return left, nil
}

func (p *jsParser) tryArrowParams() ([]string, bool) {
	if p.cur().kind != tokLParen {
		return nil, false
	}
	save := p.pos
	p.next() // '('
	var params []string
	for p.cur().kind != tokRParen {
		if p.cur().kind != tokIdent {
			p.pos = save
			return nil, false
		}
		params = append(params, p.next().lit)
		if p.cur().kind == tokComma {
			p.next()
		} else if p.cur().kind != tokRParen {
			p.pos = save
			return nil, false
		}
	}
	p.next() // ')'
	if p.cur().kind != tokArrow {
		p.pos = save
		return nil, false
	}
	p.next() // '=>'
	return params, true
}

func (p *jsParser) parseArrowBody(params []string) (jsExpr, error) {
	if p.cur().kind == tokLBrace {
		body, err := p.parseBlock()
		if err != nil {
			return nil, err
		}
		return &exprFunc{params: params, body: body, arrow: true}, nil
	}
	ex, err := p.parseAssignExpr()
	if err != nil {
		return nil, err
	}
	return &exprFunc{params: params, body: []jsStmt{&stmtReturn{value: ex}}, arrow: true}, nil
}

func (p *jsParser) parseCondExpr() (jsExpr, error) {
	test, err := p.parseBinaryExpr(1)
	if err != nil {
		return nil, err
	}
	if p.cur().kind != tokQuestion {
		return test, nil
	}
	p.next()
	cons, err := p.parseAssignExpr()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(tokColon); err != nil {
		return nil, err
	}
	alt, err := p.parseAssignExpr()
	if err != nil {
		return nil, err
	}
	return &exprCond{test: test, cons: cons, alt: alt}, nil
}

func binPrec(k jsTokKind) int {
	switch k {
	case tokNullish, tokOr:
		return 1
	case tokAnd:
		return 2
	case tokBitOr:
		return 3
	case tokBitXor:
		return 4
	case tokBitAnd:
		return 5
	case tokEq, tokNeq, tokStrictEq, tokStrictNeq:
		return 6
	case tokLt, tokLte, tokGt, tokGte, tokIn, tokInstanceof:
		return 7
	case tokShl, tokShr, tokUShr:
		return 8
	case tokPlus, tokMinus:
		return 9
	case tokStar, tokSlash, tokPercent:
		return 10
	default:
		return 0
	}
}

func (p *jsParser) parseBinaryExpr(minPrec int) (jsExpr, error) {
	left, err := p.parseUnaryExpr()
	if err != nil {
		return nil, err
	}
	for {
		prec := binPrec(p.cur().kind)
		if prec < minPrec {
			break
		}
		op := p.next().kind
		right, err := p.parseBinaryExpr(prec + 1)
		if err != nil {
			return nil, err
		}
		left = &exprBinary{op: op, left: left, right: right}
	}
	return left, nil
}

func (p *jsParser) parseUnaryExpr() (jsExpr, error) {
	switch p.cur().kind {
	case tokNot, tokMinus, tokPlus, tokBitNot, tokTypeof, tokVoid, tokDelete:
		op := p.next().kind
		arg, err := p.parseUnaryExpr()
		if err != nil {
			return nil, err
		}
		return &exprUnary{op: op, arg: arg}, nil
	case tokInc, tokDec:
		op := p.next().kind
		arg, err := p.parseUnaryExpr()
		if err != nil {
			return nil, err
		}
		return &exprUpdate{op: op, prefix: true, arg: arg}, nil
	case tokNew:
		p.next()
		callee, err := p.parseMemberExpr()
		if err != nil {
			return nil, err
		}
		var args []jsExpr
		if p.cur().kind == tokLParen {
			args, err = p.parseArgList()
			if err != nil {
				return nil, err
			}
		}
		res := jsExpr(&exprCall{callee: callee, args: args, isNew: true})
		return p.parsePostfixChain(res)
	default:
		return p.parsePostfixExpr()
	}
}

func (p *jsParser) parseMemberExpr() (jsExpr, error) {
	base, err := p.parsePrimaryExpr()
	if err != nil {
		return nil, err
	}
	for {
		switch p.cur().kind {
		case tokDot, tokOptChain:
			opt := p.next().kind == tokOptChain
			propTok := p.next()
			if propTok.lit == "" {
				return nil, fmt.Errorf("line %d: expected property name", propTok.line)
			}
			base = &exprMember{obj: base, prop: propTok.lit, optional: opt}
		case tokLBrack:
			p.next()
			idx, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(tokRBrack); err != nil {
				return nil, err
			}
			base = &exprMember{obj: base, index: idx, computed: true}
		default:
			return base, nil
		}
	}
}

func (p *jsParser) parsePostfixExpr() (jsExpr, error) {
	base, err := p.parsePrimaryExpr()
	if err != nil {
		return nil, err
	}
	return p.parsePostfixChain(base)
}

func (p *jsParser) parsePostfixChain(base jsExpr) (jsExpr, error) {
	for {
		switch p.cur().kind {
		case tokDot, tokOptChain:
			opt := p.next().kind == tokOptChain
			propTok := p.next()
			if propTok.lit == "" {
				return nil, fmt.Errorf("line %d: expected property name", propTok.line)
			}
			base = &exprMember{obj: base, prop: propTok.lit, optional: opt}
		case tokLBrack:
			p.next()
			idx, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(tokRBrack); err != nil {
				return nil, err
			}
			base = &exprMember{obj: base, index: idx, computed: true}
		case tokLParen:
			args, err := p.parseArgList()
			if err != nil {
				return nil, err
			}
			base = &exprCall{callee: base, args: args}
		case tokInc, tokDec:
			if p.cur().newline {
				return base, nil
			}
			op := p.next().kind
			base = &exprUpdate{op: op, prefix: false, arg: base}
		default:
			return base, nil
		}
	}
}

func (p *jsParser) parseArgList() ([]jsExpr, error) {
	if _, err := p.expect(tokLParen); err != nil {
		return nil, err
	}
	var args []jsExpr
	for p.cur().kind != tokRParen && p.cur().kind != tokEOF {
		a, err := p.parseAssignExpr()
		if err != nil {
			return nil, err
		}
		args = append(args, a)
		if p.cur().kind == tokComma {
			p.next()
		} else {
			break
		}
	}
	if _, err := p.expect(tokRParen); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *jsParser) parsePrimaryExpr() (jsExpr, error) {
	t := p.cur()
	switch t.kind {
	case tokIdent:
		p.next()
		return &exprIdent{name: t.lit}, nil
	case tokNumber:
		p.next()
		return &exprNumber{val: t.num}, nil
	case tokString:
		p.next()
		return &exprString{val: t.lit}, nil
	case tokTrue:
		p.next()
		return &exprBool{val: true}, nil
	case tokFalse:
		p.next()
		return &exprBool{val: false}, nil
	case tokNull:
		p.next()
		return &exprNull{}, nil
	case tokUndefined:
		p.next()
		return &exprUndefined{}, nil
	case tokThis:
		p.next()
		return &exprThis{}, nil
	case tokRegex:
		p.next()
		return &exprRegex{pattern: t.lit, flags: t.aux}, nil
	case tokLParen:
		p.next()
		ex, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokRParen); err != nil {
			return nil, err
		}
		return ex, nil
	case tokLBrack:
		p.next()
		var elems []jsExpr
		for p.cur().kind != tokRBrack && p.cur().kind != tokEOF {
			if p.cur().kind == tokComma {
				elems = append(elems, &exprUndefined{})
				p.next()
				continue
			}
			el, err := p.parseAssignExpr()
			if err != nil {
				return nil, err
			}
			elems = append(elems, el)
			if p.cur().kind == tokComma {
				p.next()
			} else {
				break
			}
		}
		if _, err := p.expect(tokRBrack); err != nil {
			return nil, err
		}
		return &exprArray{elems: elems}, nil
	case tokLBrace:
		p.next()
		var props []objProp
		for p.cur().kind != tokRBrace && p.cur().kind != tokEOF {
			kt := p.next()
			key := kt.lit
			if kt.kind == tokNumber {
				key = strconv.FormatFloat(kt.num, 'f', -1, 64)
			}
			if key == "" && kt.kind != tokString {
				return nil, fmt.Errorf("line %d: invalid object property key", kt.line)
			}
			if p.cur().kind == tokColon {
				p.next()
				val, err := p.parseAssignExpr()
				if err != nil {
					return nil, err
				}
				props = append(props, objProp{key: key, val: val})
			} else {
				// ES6 property shorthand `{ key }`
				props = append(props, objProp{key: key, val: &exprIdent{name: key}})
			}
			if p.cur().kind == tokComma {
				p.next()
			} else {
				break
			}
		}
		if _, err := p.expect(tokRBrace); err != nil {
			return nil, err
		}
		return &exprObject{props: props}, nil
	case tokFunc:
		p.next()
		var fnName string
		if p.cur().kind == tokIdent {
			fnName = p.next().lit
		}
		params, err := p.parseParamList()
		if err != nil {
			return nil, err
		}
		body, err := p.parseBlock()
		if err != nil {
			return nil, err
		}
		return &exprFunc{name: fnName, params: params, body: body}, nil
	default:
		return nil, fmt.Errorf("line %d: unexpected token %q", t.line, t.lit)
	}
}
