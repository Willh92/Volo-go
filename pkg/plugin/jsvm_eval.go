package plugin

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

type jsValKind uint8

const (
	valUndefined jsValKind = iota
	valNull
	valBool
	valNumber
	valString
	valObject
	valArray
	valFunc
	valRegExp

	maxJSArrayLen    = 65536
	maxJSStringBytes = 4 << 20 // 4 MiB string byte-length cap
)

type jsValue struct {
	kind jsValKind
	b    bool
	num  float64
	str  string
	obj  *jsObject
	arr  *jsArray
	fn   *jsFunc
	re   *jsRegExp
}

var (
	jsUndefinedVal = jsValue{kind: valUndefined}
	jsNullVal      = jsValue{kind: valNull}
	jsTrueVal      = jsValue{kind: valBool, b: true}
	jsFalseVal     = jsValue{kind: valBool, b: false}
)

type jsObject struct {
	props   map[string]jsValue
	isError bool
	isDate  bool
	epochMs float64
}

type jsArray struct {
	items []jsValue
}

type jsRegExp struct {
	pattern string
	flags   string
	global  bool
	re      *regexp.Regexp
}

type jsNativeFn func(rt *jsRuntime, thisVal jsValue, args []jsValue) (jsValue, error)

type jsFunc struct {
	name   string
	params []string
	body   []jsStmt
	env    *jsScope
	native jsNativeFn
	props  map[string]jsValue
}

type jsScope struct {
	parent *jsScope
	vars   map[string]jsValue
	consts map[string]bool
}

func newJSScope(parent *jsScope) *jsScope {
	return &jsScope{
		parent: parent,
		vars:   make(map[string]jsValue),
	}
}

func (s *jsScope) define(name string, v jsValue, isConst bool) {
	s.vars[name] = v
	if isConst {
		if s.consts == nil {
			s.consts = make(map[string]bool)
		}
		s.consts[name] = true
	}
}

func (s *jsScope) get(name string) (jsValue, bool) {
	for cur := s; cur != nil; cur = cur.parent {
		if v, ok := cur.vars[name]; ok {
			return v, true
		}
	}
	return jsUndefinedVal, false
}

func (s *jsScope) set(name string, v jsValue) error {
	for cur := s; cur != nil; cur = cur.parent {
		if _, ok := cur.vars[name]; ok {
			if cur.consts != nil && cur.consts[name] {
				return errors.New("TypeError: assignment to constant variable " + name)
			}
			cur.vars[name] = v
			return nil
		}
	}
	// Assign to root scope if undeclared (non-strict JS compatibility)
	root := s
	for root.parent != nil {
		root = root.parent
	}
	root.vars[name] = v
	return nil
}

// jsInterruptedError signals execution cancellation via Runtime.Interrupt().
type jsInterruptedError struct {
	cause any
}

func (e *jsInterruptedError) Error() string {
	return fmt.Sprintf("ExecutionInterrupted: %v", e.cause)
}

func (e *jsInterruptedError) Value() any {
	return e.cause
}

type jsThrownError struct {
	val jsValue
}

func (e *jsThrownError) Error() string {
	if e.val.kind == valObject && e.val.obj != nil && e.val.obj.isError {
		name := e.val.obj.get("name").toString()
		msg := e.val.obj.get("message").toString()
		if name != "" && msg != "" {
			return name + ": " + msg
		}
		if msg != "" {
			return msg
		}
	}
	return e.val.toString()
}

type flowKind uint8

const (
	flowNone flowKind = iota
	flowReturn
	flowBreak
	flowContinue
)

type flowResult struct {
	kind flowKind
	val  jsValue
}

type jsRuntime struct {
	global       *jsScope
	maxStack     int
	stackDepth   int
	interrupted  atomic.Bool
	interruptVal atomic.Value
}

func newJSRuntime() *jsRuntime {
	rt := &jsRuntime{
		global:   newJSScope(nil),
		maxStack: defaultVMStackLimit,
	}
	rt.installBuiltins()
	return rt
}

func (rt *jsRuntime) SetMaxCallStackSize(limit int) {
	if limit > 0 {
		rt.maxStack = limit
	}
}

func (rt *jsRuntime) Interrupt(v any) {
	if v == nil {
		v = "interrupted"
	}
	rt.interruptVal.Store(fmt.Sprint(v))
	rt.interrupted.Store(true)
}

func (rt *jsRuntime) ClearInterrupt() {
	rt.interrupted.Store(false)
}

func (rt *jsRuntime) checkInterrupt() error {
	if rt.interrupted.Load() {
		return &jsInterruptedError{cause: rt.interruptVal.Load()}
	}
	return nil
}

func (rt *jsRuntime) NewObject() *jsObject {
	return &jsObject{props: make(map[string]jsValue)}
}

func (o *jsObject) Set(key string, v any) {
	if o.props == nil {
		o.props = make(map[string]jsValue)
	}
	o.props[key] = goToJSValue(v)
}

func (o *jsObject) setVal(key string, v jsValue) {
	if o.props == nil {
		o.props = make(map[string]jsValue)
	}
	o.props[key] = v
}

func (o *jsObject) get(key string) jsValue {
	if o == nil || o.props == nil {
		return jsUndefinedVal
	}
	if v, ok := o.props[key]; ok {
		return v
	}
	return jsUndefinedVal
}

func (rt *jsRuntime) Set(name string, v any) {
	rt.global.define(name, rt.ToValue(v), false)
}

func (rt *jsRuntime) Get(name string) jsValue {
	v, _ := rt.global.get(name)
	return v
}

func (rt *jsRuntime) RunProgram(prog *jsProgram) (jsValue, error) {
	if err := rt.checkInterrupt(); err != nil {
		return jsUndefinedVal, err
	}
	rt.hoistFuncs(prog.stmts, rt.global)
	res, err := rt.execStmts(prog.stmts, rt.global, jsUndefinedVal)
	if err != nil {
		return jsUndefinedVal, err
	}
	return res.val, nil
}

func (rt *jsRuntime) hoistFuncs(stmts []jsStmt, scope *jsScope) {
	for _, st := range stmts {
		if fd, ok := st.(*stmtFuncDecl); ok {
			scope.define(fd.name, jsValue{
				kind: valFunc,
				fn: &jsFunc{
					name:   fd.name,
					params: fd.params,
					body:   fd.body,
					env:    scope,
				},
			}, false)
		}
	}
}

func (rt *jsRuntime) callFunc(fn *jsFunc, thisVal jsValue, args []jsValue) (jsValue, error) {
	if err := rt.checkInterrupt(); err != nil {
		return jsUndefinedVal, err
	}
	if fn == nil {
		return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("TypeError", "value is not a function")}
	}
	if rt.stackDepth >= rt.maxStack {
		return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("RangeError", "Maximum call stack size exceeded")}
	}
	rt.stackDepth++
	defer func() { rt.stackDepth-- }()

	if fn.native != nil {
		return fn.native(rt, thisVal, args)
	}

	callScope := newJSScope(fn.env)
	if fn.name != "" {
		callScope.define(fn.name, jsValue{kind: valFunc, fn: fn}, false)
	}
	for i, p := range fn.params {
		if i < len(args) {
			callScope.define(p, args[i], false)
		} else {
			callScope.define(p, jsUndefinedVal, false)
		}
	}
	rt.hoistFuncs(fn.body, callScope)
	res, err := rt.execStmts(fn.body, callScope, thisVal)
	if err != nil {
		return jsUndefinedVal, err
	}
	if res.kind == flowReturn {
		return res.val, nil
	}
	return jsUndefinedVal, nil
}

func (rt *jsRuntime) execStmts(stmts []jsStmt, scope *jsScope, thisVal jsValue) (flowResult, error) {
	for _, st := range stmts {
		res, err := rt.execStmt(st, scope, thisVal)
		if err != nil || res.kind != flowNone {
			return res, err
		}
	}
	return flowResult{}, nil
}

func (rt *jsRuntime) execStmt(st jsStmt, scope *jsScope, thisVal jsValue) (flowResult, error) {
	if err := rt.checkInterrupt(); err != nil {
		return flowResult{}, err
	}
	switch s := st.(type) {
	case *stmtEmpty, *stmtFuncDecl:
		return flowResult{}, nil
	case *stmtBlock:
		child := newJSScope(scope)
		rt.hoistFuncs(s.stmts, child)
		return rt.execStmts(s.stmts, child, thisVal)
	case *stmtVarDecl:
		for _, d := range s.decls {
			val := jsUndefinedVal
			if d.init != nil {
				var err error
				val, err = rt.evalExpr(d.init, scope, thisVal)
				if err != nil {
					return flowResult{}, err
				}
			}
			scope.define(d.name, val, s.kind == tokConst)
		}
		return flowResult{}, nil
	case *stmtExpr:
		_, err := rt.evalExpr(s.expr, scope, thisVal)
		return flowResult{}, err
	case *stmtReturn:
		if s.value == nil {
			return flowResult{kind: flowReturn, val: jsUndefinedVal}, nil
		}
		v, err := rt.evalExpr(s.value, scope, thisVal)
		if err != nil {
			return flowResult{}, err
		}
		return flowResult{kind: flowReturn, val: v}, nil
	case *stmtBreak:
		return flowResult{kind: flowBreak}, nil
	case *stmtContinue:
		return flowResult{kind: flowContinue}, nil
	case *stmtThrow:
		v, err := rt.evalExpr(s.value, scope, thisVal)
		if err != nil {
			return flowResult{}, err
		}
		return flowResult{}, &jsThrownError{val: v}
	case *stmtIf:
		c, err := rt.evalExpr(s.cond, scope, thisVal)
		if err != nil {
			return flowResult{}, err
		}
		if c.toBool() {
			return rt.execStmt(s.then, scope, thisVal)
		}
		if s.alt != nil {
			return rt.execStmt(s.alt, scope, thisVal)
		}
		return flowResult{}, nil
	case *stmtWhile:
		for {
			if err := rt.checkInterrupt(); err != nil {
				return flowResult{}, err
			}
			c, err := rt.evalExpr(s.cond, scope, thisVal)
			if err != nil || !c.toBool() {
				return flowResult{}, err
			}
			res, err := rt.execStmt(s.body, scope, thisVal)
			if err != nil {
				return flowResult{}, err
			}
			if res.kind == flowBreak {
				return flowResult{}, nil
			}
			if res.kind == flowReturn {
				return res, nil
			}
		}
	case *stmtDoWhile:
		for {
			if err := rt.checkInterrupt(); err != nil {
				return flowResult{}, err
			}
			res, err := rt.execStmt(s.body, scope, thisVal)
			if err != nil {
				return flowResult{}, err
			}
			if res.kind == flowBreak {
				return flowResult{}, nil
			}
			if res.kind == flowReturn {
				return res, nil
			}
			c, err := rt.evalExpr(s.cond, scope, thisVal)
			if err != nil || !c.toBool() {
				return flowResult{}, err
			}
		}
	case *stmtFor:
		loopScope := newJSScope(scope)
		if s.init != nil {
			if _, err := rt.execStmt(s.init, loopScope, thisVal); err != nil {
				return flowResult{}, err
			}
		}
		for {
			if err := rt.checkInterrupt(); err != nil {
				return flowResult{}, err
			}
			if s.cond != nil {
				c, err := rt.evalExpr(s.cond, loopScope, thisVal)
				if err != nil || !c.toBool() {
					return flowResult{}, err
				}
			}
			res, err := rt.execStmt(s.body, loopScope, thisVal)
			if err != nil {
				return flowResult{}, err
			}
			if res.kind == flowBreak {
				return flowResult{}, nil
			}
			if res.kind == flowReturn {
				return res, nil
			}
			if s.post != nil {
				if _, err := rt.evalExpr(s.post, loopScope, thisVal); err != nil {
					return flowResult{}, err
				}
			}
		}
	case *stmtForInOf:
		target, err := rt.evalExpr(s.right, scope, thisVal)
		if err != nil {
			return flowResult{}, err
		}
		var iterVals []jsValue
		if s.isOf {
			if target.kind == valArray && target.arr != nil {
				iterVals = append(iterVals, target.arr.items...)
			} else if target.kind == valString {
				for _, r := range target.str {
					iterVals = append(iterVals, jsValue{kind: valString, str: string(r)})
				}
			}
		} else {
			if target.kind == valObject && target.obj != nil {
				for k := range target.obj.props {
					iterVals = append(iterVals, jsValue{kind: valString, str: k})
				}
			} else if target.kind == valArray && target.arr != nil {
				for idx := range target.arr.items {
					iterVals = append(iterVals, jsValue{kind: valString, str: strconv.Itoa(idx)})
				}
			}
		}
		for _, iv := range iterVals {
			if err := rt.checkInterrupt(); err != nil {
				return flowResult{}, err
			}
			iterScope := newJSScope(scope)
			iterScope.define(s.varName, iv, false)
			res, err := rt.execStmt(s.body, iterScope, thisVal)
			if err != nil {
				return flowResult{}, err
			}
			if res.kind == flowBreak {
				break
			}
			if res.kind == flowReturn {
				return res, nil
			}
		}
		return flowResult{}, nil
	case *stmtSwitch:
		disc, err := rt.evalExpr(s.disc, scope, thisVal)
		if err != nil {
			return flowResult{}, err
		}
		matched := false
		defaultIdx := -1
		for idx, cl := range s.cases {
			if cl.test == nil {
				defaultIdx = idx
				continue
			}
			if !matched {
				tv, err := rt.evalExpr(cl.test, scope, thisVal)
				if err != nil {
					return flowResult{}, err
				}
				if jsStrictEqual(disc, tv) {
					matched = true
				}
			}
			if matched {
				res, err := rt.execStmts(cl.stmts, scope, thisVal)
				if err != nil {
					return flowResult{}, err
				}
				if res.kind == flowBreak {
					return flowResult{}, nil
				}
				if res.kind == flowReturn {
					return res, nil
				}
			}
		}
		if !matched && defaultIdx >= 0 {
			for idx := defaultIdx; idx < len(s.cases); idx++ {
				res, err := rt.execStmts(s.cases[idx].stmts, scope, thisVal)
				if err != nil {
					return flowResult{}, err
				}
				if res.kind == flowBreak {
					return flowResult{}, nil
				}
				if res.kind == flowReturn {
					return res, nil
				}
			}
		}
		return flowResult{}, nil
	case *stmtTry:
		res, err := rt.execStmts(s.block, newJSScope(scope), thisVal)
		var inter *jsInterruptedError
		if errors.As(err, &inter) {
			return flowResult{}, err
		}
		if err != nil && s.hasCatch {
			catchScope := newJSScope(scope)
			if s.catchName != "" {
				var thrown *jsThrownError
				if errors.As(err, &thrown) {
					catchScope.define(s.catchName, thrown.val, false)
				} else {
					catchScope.define(s.catchName, rt.newErrorVal("Error", err.Error()), false)
				}
			}
			res, err = rt.execStmts(s.handler, catchScope, thisVal)
		}
		if len(s.finalizer) > 0 {
			fRes, fErr := rt.execStmts(s.finalizer, newJSScope(scope), thisVal)
			if fErr != nil {
				return flowResult{}, fErr
			}
			if fRes.kind != flowNone {
				return fRes, nil
			}
		}
		return res, err
	}
	return flowResult{}, nil
}

func (rt *jsRuntime) evalExpr(ex jsExpr, scope *jsScope, thisVal jsValue) (jsValue, error) {
	if err := rt.checkInterrupt(); err != nil {
		return jsUndefinedVal, err
	}
	switch e := ex.(type) {
	case *exprUndefined:
		return jsUndefinedVal, nil
	case *exprNull:
		return jsNullVal, nil
	case *exprBool:
		if e.val {
			return jsTrueVal, nil
		}
		return jsFalseVal, nil
	case *exprNumber:
		return jsValue{kind: valNumber, num: e.val}, nil
	case *exprString:
		return jsValue{kind: valString, str: e.val}, nil
	case *exprThis:
		return thisVal, nil
	case *exprRegex:
		return rt.compileRegExp(e.pattern, e.flags)
	case *exprIdent:
		v, ok := scope.get(e.name)
		if !ok {
			return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("ReferenceError", e.name+" is not defined")}
		}
		return v, nil
	case *exprArray:
		if len(e.elems) > maxJSArrayLen {
			return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("RangeError", "Invalid array length")}
		}
		items := make([]jsValue, len(e.elems))
		for i, el := range e.elems {
			v, err := rt.evalExpr(el, scope, thisVal)
			if err != nil {
				return jsUndefinedVal, err
			}
			items[i] = v
		}
		return jsValue{kind: valArray, arr: &jsArray{items: items}}, nil
	case *exprObject:
		obj := rt.NewObject()
		for _, pr := range e.props {
			v, err := rt.evalExpr(pr.val, scope, thisVal)
			if err != nil {
				return jsUndefinedVal, err
			}
			obj.setVal(pr.key, v)
		}
		return jsValue{kind: valObject, obj: obj}, nil
	case *exprFunc:
		return jsValue{
			kind: valFunc,
			fn: &jsFunc{
				name:   e.name,
				params: e.params,
				body:   e.body,
				env:    scope,
			},
		}, nil
	case *exprSeq:
		var last jsValue
		var err error
		for _, sub := range e.exprs {
			last, err = rt.evalExpr(sub, scope, thisVal)
			if err != nil {
				return jsUndefinedVal, err
			}
		}
		return last, nil
	case *exprCond:
		t, err := rt.evalExpr(e.test, scope, thisVal)
		if err != nil {
			return jsUndefinedVal, err
		}
		if t.toBool() {
			return rt.evalExpr(e.cons, scope, thisVal)
		}
		return rt.evalExpr(e.alt, scope, thisVal)
	case *exprUnary:
		if e.op == tokTypeof {
			if id, ok := e.arg.(*exprIdent); ok {
				v, exists := scope.get(id.name)
				if !exists {
					return jsValue{kind: valString, str: "undefined"}, nil
				}
				return jsValue{kind: valString, str: v.typeOf()}, nil
			}
			v, err := rt.evalExpr(e.arg, scope, thisVal)
			if err != nil {
				return jsUndefinedVal, err
			}
			return jsValue{kind: valString, str: v.typeOf()}, nil
		}
		if e.op == tokDelete {
			if mem, ok := e.arg.(*exprMember); ok {
				objVal, err := rt.evalExpr(mem.obj, scope, thisVal)
				if err != nil {
					return jsUndefinedVal, err
				}
				key := mem.prop
				if mem.computed {
					kv, err := rt.evalExpr(mem.index, scope, thisVal)
					if err != nil {
						return jsUndefinedVal, err
					}
					key = kv.toString()
				}
				if objVal.kind == valObject && objVal.obj != nil {
					delete(objVal.obj.props, key)
				}
			}
			return jsTrueVal, nil
		}
		v, err := rt.evalExpr(e.arg, scope, thisVal)
		if err != nil {
			return jsUndefinedVal, err
		}
		switch e.op {
		case tokNot:
			if !v.toBool() {
				return jsTrueVal, nil
			}
			return jsFalseVal, nil
		case tokMinus:
			return jsValue{kind: valNumber, num: -v.toNumber()}, nil
		case tokPlus:
			return jsValue{kind: valNumber, num: v.toNumber()}, nil
		case tokBitNot:
			return jsValue{kind: valNumber, num: float64(^int32(v.toNumber()))}, nil
		case tokVoid:
			return jsUndefinedVal, nil
		}
	case *exprUpdate:
		cur, setter, err := rt.resolveLValue(e.arg, scope, thisVal)
		if err != nil {
			return jsUndefinedVal, err
		}
		oldNum := cur.toNumber()
		newNum := oldNum + 1
		if e.op == tokDec {
			newNum = oldNum - 1
		}
		nextVal := jsValue{kind: valNumber, num: newNum}
		if err := setter(nextVal); err != nil {
			var thrown *jsThrownError
			var inter *jsInterruptedError
			if errors.As(err, &thrown) || errors.As(err, &inter) {
				return jsUndefinedVal, err
			}
			return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("TypeError", err.Error())}
		}
		if e.prefix {
			return nextVal, nil
		}
		return jsValue{kind: valNumber, num: oldNum}, nil
	case *exprAssign:
		cur, setter, err := rt.resolveLValue(e.left, scope, thisVal)
		if err != nil {
			return jsUndefinedVal, err
		}
		rhs, err := rt.evalExpr(e.right, scope, thisVal)
		if err != nil {
			return jsUndefinedVal, err
		}
		var out jsValue
		switch e.op {
		case tokAssign:
			out = rhs
		case tokPlusAssign:
			if cur.kind == valString || rhs.kind == valString {
				ls := cur.toString()
				rs := rhs.toString()
				if len(ls)+len(rs) > maxJSStringBytes {
					return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("RangeError", "Invalid string length")}
				}
				out = jsValue{kind: valString, str: ls + rs}
			} else {
				out = jsValue{kind: valNumber, num: cur.toNumber() + rhs.toNumber()}
			}
		case tokMinusAssign:
			out = jsValue{kind: valNumber, num: cur.toNumber() - rhs.toNumber()}
		case tokStarAssign:
			out = jsValue{kind: valNumber, num: cur.toNumber() * rhs.toNumber()}
		case tokSlashAssign:
			out = jsValue{kind: valNumber, num: cur.toNumber() / rhs.toNumber()}
		case tokPctAssign:
			out = jsValue{kind: valNumber, num: math.Mod(cur.toNumber(), rhs.toNumber())}
		}
		if err := setter(out); err != nil {
			var thrown *jsThrownError
			var inter *jsInterruptedError
			if errors.As(err, &thrown) || errors.As(err, &inter) {
				return jsUndefinedVal, err
			}
			return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("TypeError", err.Error())}
		}
		return out, nil
	case *exprBinary:
		if e.op == tokAnd {
			lv, err := rt.evalExpr(e.left, scope, thisVal)
			if err != nil || !lv.toBool() {
				return lv, err
			}
			return rt.evalExpr(e.right, scope, thisVal)
		}
		if e.op == tokOr {
			lv, err := rt.evalExpr(e.left, scope, thisVal)
			if err != nil || lv.toBool() {
				return lv, err
			}
			return rt.evalExpr(e.right, scope, thisVal)
		}
		if e.op == tokNullish {
			lv, err := rt.evalExpr(e.left, scope, thisVal)
			if err != nil {
				return jsUndefinedVal, err
			}
			if lv.kind != valUndefined && lv.kind != valNull {
				return lv, nil
			}
			return rt.evalExpr(e.right, scope, thisVal)
		}
		lv, err := rt.evalExpr(e.left, scope, thisVal)
		if err != nil {
			return jsUndefinedVal, err
		}
		rv, err := rt.evalExpr(e.right, scope, thisVal)
		if err != nil {
			return jsUndefinedVal, err
		}
		return rt.evalBinaryOp(e.op, lv, rv)
	case *exprMember:
		objVal, err := rt.evalExpr(e.obj, scope, thisVal)
		if err != nil {
			return jsUndefinedVal, err
		}
		if objVal.kind == valUndefined || objVal.kind == valNull {
			if e.optional {
				return jsUndefinedVal, nil
			}
			return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("TypeError", "Cannot read properties of "+objVal.toString())}
		}
		prop := e.prop
		if e.computed {
			kv, err := rt.evalExpr(e.index, scope, thisVal)
			if err != nil {
				return jsUndefinedVal, err
			}
			prop = kv.toString()
		}
		return rt.getProperty(objVal, prop), nil
	case *exprCall:
		var receiver = jsUndefinedVal
		var calleeVal jsValue
		var err error
		if mem, ok := e.callee.(*exprMember); ok {
			receiver, err = rt.evalExpr(mem.obj, scope, thisVal)
			if err != nil {
				return jsUndefinedVal, err
			}
			if receiver.kind == valUndefined || receiver.kind == valNull {
				if mem.optional {
					return jsUndefinedVal, nil
				}
				return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("TypeError", "Cannot read properties of "+receiver.toString())}
			}
			prop := mem.prop
			if mem.computed {
				kv, err := rt.evalExpr(mem.index, scope, thisVal)
				if err != nil {
					return jsUndefinedVal, err
				}
				prop = kv.toString()
			}
			calleeVal = rt.getProperty(receiver, prop)
		} else {
			calleeVal, err = rt.evalExpr(e.callee, scope, thisVal)
			if err != nil {
				return jsUndefinedVal, err
			}
		}
		argVals := make([]jsValue, len(e.args))
		for i, a := range e.args {
			argVals[i], err = rt.evalExpr(a, scope, thisVal)
			if err != nil {
				return jsUndefinedVal, err
			}
		}
		if e.isNew && receiver.kind == valUndefined {
			if calleeVal.kind != valFunc || calleeVal.fn == nil {
				return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("TypeError", calleeVal.toString()+" is not a constructor")}
			}
			newThis := jsValue{kind: valObject, obj: rt.NewObject()}
			res, err := rt.callFunc(calleeVal.fn, newThis, argVals)
			if err != nil {
				return jsUndefinedVal, err
			}
			if res.kind == valObject || res.kind == valArray || res.kind == valRegExp {
				return res, nil
			}
			return newThis, nil
		}
		return rt.callFunc(calleeVal.fn, receiver, argVals)
	}
	return jsUndefinedVal, nil
}

func (rt *jsRuntime) resolveLValue(target jsExpr, scope *jsScope, thisVal jsValue) (jsValue, func(jsValue) error, error) {
	switch lv := target.(type) {
	case *exprIdent:
		cur, _ := scope.get(lv.name)
		return cur, func(v jsValue) error { return scope.set(lv.name, v) }, nil
	case *exprMember:
		objVal, err := rt.evalExpr(lv.obj, scope, thisVal)
		if err != nil {
			return jsUndefinedVal, nil, err
		}
		prop := lv.prop
		if lv.computed {
			kv, err := rt.evalExpr(lv.index, scope, thisVal)
			if err != nil {
				return jsUndefinedVal, nil, err
			}
			prop = kv.toString()
		}
		cur := rt.getProperty(objVal, prop)
		return cur, func(v jsValue) error {
			if objVal.kind == valObject && objVal.obj != nil {
				objVal.obj.setVal(prop, v)
				return nil
			}
			if objVal.kind == valArray && objVal.arr != nil {
				if idx, pErr := strconv.Atoi(prop); pErr == nil && idx >= 0 {
					if idx >= maxJSArrayLen {
						return &jsThrownError{val: rt.newErrorVal("RangeError", "Invalid array length")}
					}
					for len(objVal.arr.items) <= idx {
						if iErr := rt.checkInterrupt(); iErr != nil {
							return iErr
						}
						objVal.arr.items = append(objVal.arr.items, jsUndefinedVal)
					}
					objVal.arr.items[idx] = v
					return nil
				}
			}
			return errors.New("Cannot set property " + prop)
		}, nil
	default:
		return jsUndefinedVal, nil, &jsThrownError{val: rt.newErrorVal("ReferenceError", "Invalid left-hand side in assignment")}
	}
}

func (rt *jsRuntime) evalBinaryOp(op jsTokKind, l, r jsValue) (jsValue, error) {
	switch op {
	case tokPlus:
		if l.kind == valString || r.kind == valString {
			ls := l.toString()
			rs := r.toString()
			if len(ls)+len(rs) > maxJSStringBytes {
				return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("RangeError", "Invalid string length")}
			}
			return jsValue{kind: valString, str: ls + rs}, nil
		}
		return jsValue{kind: valNumber, num: l.toNumber() + r.toNumber()}, nil
	case tokMinus:
		return jsValue{kind: valNumber, num: l.toNumber() - r.toNumber()}, nil
	case tokStar:
		return jsValue{kind: valNumber, num: l.toNumber() * r.toNumber()}, nil
	case tokSlash:
		return jsValue{kind: valNumber, num: l.toNumber() / r.toNumber()}, nil
	case tokPercent:
		return jsValue{kind: valNumber, num: math.Mod(l.toNumber(), r.toNumber())}, nil
	case tokStrictEq:
		return boolVal(jsStrictEqual(l, r)), nil
	case tokStrictNeq:
		return boolVal(!jsStrictEqual(l, r)), nil
	case tokEq:
		return boolVal(jsLooseEqual(l, r)), nil
	case tokNeq:
		return boolVal(!jsLooseEqual(l, r)), nil
	case tokLt:
		if l.kind == valString && r.kind == valString {
			return boolVal(l.str < r.str), nil
		}
		return boolVal(l.toNumber() < r.toNumber()), nil
	case tokLte:
		if l.kind == valString && r.kind == valString {
			return boolVal(l.str <= r.str), nil
		}
		return boolVal(l.toNumber() <= r.toNumber()), nil
	case tokGt:
		if l.kind == valString && r.kind == valString {
			return boolVal(l.str > r.str), nil
		}
		return boolVal(l.toNumber() > r.toNumber()), nil
	case tokGte:
		if l.kind == valString && r.kind == valString {
			return boolVal(l.str >= r.str), nil
		}
		return boolVal(l.toNumber() >= r.toNumber()), nil
	case tokBitAnd:
		return jsValue{kind: valNumber, num: float64(int32(l.toNumber()) & int32(r.toNumber()))}, nil
	case tokBitOr:
		return jsValue{kind: valNumber, num: float64(int32(l.toNumber()) | int32(r.toNumber()))}, nil
	case tokBitXor:
		return jsValue{kind: valNumber, num: float64(int32(l.toNumber()) ^ int32(r.toNumber()))}, nil
	case tokShl:
		return jsValue{kind: valNumber, num: float64(int32(l.toNumber()) << (uint32(r.toNumber()) & 31))}, nil
	case tokShr:
		return jsValue{kind: valNumber, num: float64(int32(l.toNumber()) >> (uint32(r.toNumber()) & 31))}, nil
	case tokUShr:
		return jsValue{kind: valNumber, num: float64(uint32(l.toNumber()) >> (uint32(r.toNumber()) & 31))}, nil
	case tokIn:
		if r.kind == valObject && r.obj != nil {
			_, ok := r.obj.props[l.toString()]
			return boolVal(ok), nil
		}
		if r.kind == valArray && r.arr != nil {
			idx, err := strconv.Atoi(l.toString())
			return boolVal(err == nil && idx >= 0 && idx < len(r.arr.items)), nil
		}
		return jsFalseVal, nil
	case tokInstanceof:
		if l.kind == valObject && l.obj != nil && l.obj.isError {
			return jsTrueVal, nil
		}
		return jsFalseVal, nil
	}
	return jsUndefinedVal, nil
}

func boolVal(b bool) jsValue {
	if b {
		return jsTrueVal
	}
	return jsFalseVal
}

func jsStrictEqual(a, b jsValue) bool {
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case valUndefined, valNull:
		return true
	case valBool:
		return a.b == b.b
	case valNumber:
		return a.num == b.num
	case valString:
		return a.str == b.str
	case valObject:
		return a.obj == b.obj
	case valArray:
		return a.arr == b.arr
	case valFunc:
		return a.fn == b.fn
	case valRegExp:
		return a.re == b.re
	}
	return false
}

func jsLooseEqual(a, b jsValue) bool {
	if a.kind == b.kind {
		return jsStrictEqual(a, b)
	}
	if (a.kind == valNull && b.kind == valUndefined) || (a.kind == valUndefined && b.kind == valNull) {
		return true
	}
	if a.kind == valNumber && b.kind == valString {
		return a.num == b.toNumber()
	}
	if a.kind == valString && b.kind == valNumber {
		return a.toNumber() == b.num
	}
	if a.kind == valBool {
		return jsLooseEqual(jsValue{kind: valNumber, num: a.toNumber()}, b)
	}
	if b.kind == valBool {
		return jsLooseEqual(a, jsValue{kind: valNumber, num: b.toNumber()})
	}
	return false
}

// --- Value Coercion & Built-in Methods ---

func (v jsValue) toBool() bool {
	switch v.kind {
	case valUndefined, valNull:
		return false
	case valBool:
		return v.b
	case valNumber:
		return v.num != 0 && !math.IsNaN(v.num)
	case valString:
		return v.str != ""
	default:
		return true
	}
}

func (v jsValue) toNumber() float64 {
	switch v.kind {
	case valUndefined:
		return math.NaN()
	case valNull:
		return 0
	case valBool:
		if v.b {
			return 1
		}
		return 0
	case valNumber:
		return v.num
	case valString:
		s := strings.TrimSpace(v.str)
		if s == "" {
			return 0
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return math.NaN()
		}
		return f
	case valObject:
		if v.obj != nil && v.obj.isDate {
			return v.obj.epochMs
		}
		return math.NaN()
	default:
		return math.NaN()
	}
}

func (v jsValue) toString() string {
	switch v.kind {
	case valUndefined:
		return "undefined"
	case valNull:
		return "null"
	case valBool:
		if v.b {
			return "true"
		}
		return "false"
	case valNumber:
		if math.IsNaN(v.num) {
			return "NaN"
		}
		if math.IsInf(v.num, 1) {
			return "Infinity"
		}
		if math.IsInf(v.num, -1) {
			return "-Infinity"
		}
		return strconv.FormatFloat(v.num, 'f', -1, 64)
	case valString:
		return v.str
	case valArray:
		if v.arr == nil {
			return ""
		}
		parts := make([]string, len(v.arr.items))
		for i, it := range v.arr.items {
			if it.kind != valUndefined && it.kind != valNull {
				parts[i] = it.toString()
			}
		}
		return strings.Join(parts, ",")
	case valObject:
		if v.obj != nil {
			if v.obj.isError {
				name := v.obj.get("name").toString()
				msg := v.obj.get("message").toString()
				if msg != "" {
					return name + ": " + msg
				}
				return name
			}
			if v.obj.isDate {
				if math.IsNaN(v.obj.epochMs) || math.IsInf(v.obj.epochMs, 0) {
					return "Invalid Date"
				}
				return time.UnixMilli(int64(v.obj.epochMs)).UTC().Format(time.RFC1123)
			}
		}
		return "[object Object]"
	default:
		return "[object]"
	}
}

func (v jsValue) typeOf() string {
	switch v.kind {
	case valUndefined:
		return "undefined"
	case valBool:
		return "boolean"
	case valNumber:
		return "number"
	case valString:
		return "string"
	case valFunc:
		return "function"
	default:
		return "object"
	}
}

func (rt *jsRuntime) newErrorVal(name, msg string) jsValue {
	o := rt.NewObject()
	o.isError = true
	o.setVal("name", jsValue{kind: valString, str: name})
	o.setVal("message", jsValue{kind: valString, str: msg})
	return jsValue{kind: valObject, obj: o}
}

func (rt *jsRuntime) compileRegExp(pattern, flags string) (jsValue, error) {
	prefix := ""
	global := false
	for _, f := range flags {
		switch f {
		case 'g':
			global = true
		case 'i':
			prefix += "i"
		case 'm':
			prefix += "m"
		case 's':
			prefix += "s"
		}
	}
	full := pattern
	if prefix != "" {
		full = "(?" + prefix + ")" + pattern
	}
	re, err := regexp.Compile(full)
	if err != nil {
		return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("SyntaxError", "Invalid RegExp: "+err.Error())}
	}
	return jsValue{
		kind: valRegExp,
		re:   &jsRegExp{pattern: pattern, flags: flags, global: global, re: re},
	}, nil
}

func (rt *jsRuntime) getProperty(target jsValue, prop string) jsValue {
	switch target.kind {
	case valObject:
		if target.obj != nil {
			if prop == "toString" && target.obj.isError {
				return nativeVal(func(_ *jsRuntime, thisVal jsValue, _ []jsValue) (jsValue, error) {
					return jsValue{kind: valString, str: thisVal.toString()}, nil
				})
			}
			return target.obj.get(prop)
		}
	case valFunc:
		if target.fn != nil && target.fn.props != nil {
			if v, ok := target.fn.props[prop]; ok {
				return v
			}
		}
	case valString:
		return stringProperty(target.str, prop)
	case valArray:
		if target.arr != nil {
			return arrayProperty(target.arr, prop)
		}
	case valRegExp:
		if target.re != nil {
			return regexpProperty(target.re, prop)
		}
	}
	return jsUndefinedVal
}

func nativeVal(fn jsNativeFn) jsValue {
	return jsValue{kind: valFunc, fn: &jsFunc{native: fn}}
}

func stringProperty(s string, prop string) jsValue {
	if prop == "length" {
		return jsValue{kind: valNumber, num: float64(utf8.RuneCountInString(s))}
	}
	switch prop {
	case "toLowerCase":
		return nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
			return jsValue{kind: valString, str: strings.ToLower(s)}, nil
		})
	case "toUpperCase":
		return nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
			return jsValue{kind: valString, str: strings.ToUpper(s)}, nil
		})
	case "trim":
		return nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
			return jsValue{kind: valString, str: strings.TrimSpace(s)}, nil
		})
	case "indexOf":
		return nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 {
				return jsValue{kind: valNumber, num: -1}, nil
			}
			sub := args[0].toString()
			byteIdx := strings.Index(s, sub)
			if byteIdx < 0 {
				return jsValue{kind: valNumber, num: -1}, nil
			}
			return jsValue{kind: valNumber, num: float64(utf8.RuneCountInString(s[:byteIdx]))}, nil
		})
	case "includes":
		return nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 {
				return jsFalseVal, nil
			}
			return boolVal(strings.Contains(s, args[0].toString())), nil
		})
	case "startsWith":
		return nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 {
				return jsFalseVal, nil
			}
			return boolVal(strings.HasPrefix(s, args[0].toString())), nil
		})
	case "endsWith":
		return nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 {
				return jsFalseVal, nil
			}
			return boolVal(strings.HasSuffix(s, args[0].toString())), nil
		})
	case "slice", "substring":
		return nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			runes := []rune(s)
			l := len(runes)
			start := 0
			end := l
			if len(args) >= 1 && args[0].kind != valUndefined {
				start = int(args[0].toNumber())
				if start < 0 {
					if prop == "slice" {
						start = max(0, l+start)
					} else {
						start = 0
					}
				}
				if start > l {
					start = l
				}
			}
			if len(args) >= 2 && args[1].kind != valUndefined {
				end = int(args[1].toNumber())
				if end < 0 {
					if prop == "slice" {
						end = max(0, l+end)
					} else {
						end = 0
					}
				}
				if end > l {
					end = l
				}
			}
			if end < start {
				if prop == "substring" {
					start, end = end, start
				} else {
					return jsValue{kind: valString, str: ""}, nil
				}
			}
			return jsValue{kind: valString, str: string(runes[start:end])}, nil
		})
	case "charAt":
		return nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			idx := 0
			if len(args) > 0 {
				idx = int(args[0].toNumber())
			}
			if idx < 0 {
				return jsValue{kind: valString, str: ""}, nil
			}
			runes := []rune(s)
			if idx >= len(runes) {
				return jsValue{kind: valString, str: ""}, nil
			}
			return jsValue{kind: valString, str: string(runes[idx])}, nil
		})
	case "charCodeAt":
		return nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			idx := 0
			if len(args) > 0 {
				idx = int(args[0].toNumber())
			}
			if idx < 0 {
				return jsValue{kind: valNumber, num: math.NaN()}, nil
			}
			runes := []rune(s)
			if idx >= len(runes) {
				return jsValue{kind: valNumber, num: math.NaN()}, nil
			}
			return jsValue{kind: valNumber, num: float64(runes[idx])}, nil
		})
	case "repeat":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 || args[0].kind == valUndefined {
				return jsValue{kind: valString, str: ""}, nil
			}
			n := args[0].toNumber()
			if math.IsNaN(n) {
				return jsValue{kind: valString, str: ""}, nil
			}
			if n < 0 || math.IsInf(n, 0) {
				return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("RangeError", "Invalid count value")}
			}
			count := int(n)
			if count == 0 || len(s) == 0 {
				return jsValue{kind: valString, str: ""}, nil
			}
			if count > maxJSStringBytes || int64(len(s))*int64(count) > maxJSStringBytes {
				return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("RangeError", "Invalid string length")}
			}
			return jsValue{kind: valString, str: strings.Repeat(s, count)}, nil
		})
	case "split":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			var parts []string
			if len(args) == 0 || args[0].kind == valUndefined {
				parts = []string{s}
			} else if args[0].kind == valRegExp && args[0].re != nil {
				parts = args[0].re.re.Split(s, maxJSArrayLen+2)
			} else {
				parts = strings.SplitN(s, args[0].toString(), maxJSArrayLen+2)
			}
			if len(parts) > maxJSArrayLen {
				return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("RangeError", "Invalid array length")}
			}
			items := make([]jsValue, len(parts))
			for i, p := range parts {
				items[i] = jsValue{kind: valString, str: p}
			}
			return jsValue{kind: valArray, arr: &jsArray{items: items}}, nil
		})
	case "replace", "replaceAll":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) < 2 {
				return jsValue{kind: valString, str: s}, nil
			}
			repl := args[1].toString()
			if args[0].kind == valRegExp && args[0].re != nil {
				rx := args[0].re
				if rx.global || prop == "replaceAll" {
					var out string
					if strings.Contains(repl, "$") {
						out = rx.re.ReplaceAllString(s, repl)
					} else {
						exceeded := false
						var grown int64
						out = rx.re.ReplaceAllStringFunc(s, func(matched string) string {
							grown += int64(len(repl)) - int64(len(matched))
							if int64(len(s))+grown > maxJSStringBytes {
								exceeded = true
								return ""
							}
							return repl
						})
						if exceeded {
							return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("RangeError", "Invalid string length")}
						}
					}
					if len(out) > maxJSStringBytes {
						return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("RangeError", "Invalid string length")}
					}
					return jsValue{kind: valString, str: out}, nil
				}
				loc := rx.re.FindStringIndex(s)
				if loc == nil {
					return jsValue{kind: valString, str: s}, nil
				}
				if len(s)-(loc[1]-loc[0])+len(repl) > maxJSStringBytes {
					return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("RangeError", "Invalid string length")}
				}
				return jsValue{kind: valString, str: s[:loc[0]] + repl + s[loc[1]:]}, nil
			}
			pat := args[0].toString()
			if prop == "replaceAll" {
				if pat == "" {
					projected := int64(len(s)) + (int64(utf8.RuneCountInString(s))+1)*int64(len(repl))
					if projected > maxJSStringBytes {
						return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("RangeError", "Invalid string length")}
					}
				} else if cnt := strings.Count(s, pat); cnt > 0 {
					projected := int64(len(s)) + int64(cnt)*(int64(len(repl))-int64(len(pat)))
					if projected > maxJSStringBytes {
						return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("RangeError", "Invalid string length")}
					}
				}
				return jsValue{kind: valString, str: strings.ReplaceAll(s, pat, repl)}, nil
			}
			out := strings.Replace(s, pat, repl, 1)
			if len(out) > maxJSStringBytes {
				return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("RangeError", "Invalid string length")}
			}
			return jsValue{kind: valString, str: out}, nil
		})
	case "match":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 || args[0].kind != valRegExp || args[0].re == nil {
				return jsNullVal, nil
			}
			rx := args[0].re
			if rx.global {
				matches := rx.re.FindAllString(s, maxJSArrayLen+1)
				if matches == nil {
					return jsNullVal, nil
				}
				if len(matches) > maxJSArrayLen {
					return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("RangeError", "Invalid array length")}
				}
				items := make([]jsValue, len(matches))
				for i, m := range matches {
					items[i] = jsValue{kind: valString, str: m}
				}
				return jsValue{kind: valArray, arr: &jsArray{items: items}}, nil
			}
			sub := rx.re.FindStringSubmatch(s)
			if sub == nil {
				return jsNullVal, nil
			}
			if len(sub) > maxJSArrayLen {
				return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("RangeError", "Invalid array length")}
			}
			items := make([]jsValue, len(sub))
			for i, m := range sub {
				items[i] = jsValue{kind: valString, str: m}
			}
			return jsValue{kind: valArray, arr: &jsArray{items: items}}, nil
		})
	}
	if idx, err := strconv.Atoi(prop); err == nil && idx >= 0 {
		runes := []rune(s)
		if idx < len(runes) {
			return jsValue{kind: valString, str: string(runes[idx])}
		}
	}
	return jsUndefinedVal
}

func arrayProperty(arr *jsArray, prop string) jsValue {
	if prop == "length" {
		return jsValue{kind: valNumber, num: float64(len(arr.items))}
	}
	if idx, err := strconv.Atoi(prop); err == nil {
		if idx >= 0 && idx < len(arr.items) {
			return arr.items[idx]
		}
		return jsUndefinedVal
	}
	switch prop {
	case "push":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(arr.items)+len(args) > maxJSArrayLen {
				return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("RangeError", "Invalid array length")}
			}
			arr.items = append(arr.items, args...)
			return jsValue{kind: valNumber, num: float64(len(arr.items))}, nil
		})
	case "pop":
		return nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
			if len(arr.items) == 0 {
				return jsUndefinedVal, nil
			}
			last := arr.items[len(arr.items)-1]
			arr.items[len(arr.items)-1] = jsUndefinedVal
			arr.items = arr.items[:len(arr.items)-1]
			return last, nil
		})
	case "join":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			sep := ","
			if len(args) > 0 && args[0].kind != valUndefined {
				sep = args[0].toString()
			}
			if len(arr.items) == 0 {
				return jsValue{kind: valString, str: ""}, nil
			}
			var totalBytes int64
			if len(arr.items) > 1 && len(sep) > 0 {
				totalBytes = int64(len(arr.items)-1) * int64(len(sep))
				if totalBytes > maxJSStringBytes {
					return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("RangeError", "Invalid string length")}
				}
			}
			parts := make([]string, len(arr.items))
			for i, it := range arr.items {
				if err := rt.checkInterrupt(); err != nil {
					return jsUndefinedVal, err
				}
				if it.kind != valUndefined && it.kind != valNull {
					p := it.toString()
					totalBytes += int64(len(p))
					if totalBytes > maxJSStringBytes {
						return jsUndefinedVal, &jsThrownError{val: rt.newErrorVal("RangeError", "Invalid string length")}
					}
					parts[i] = p
				}
			}
			return jsValue{kind: valString, str: strings.Join(parts, sep)}, nil
		})
	case "slice":
		return nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			l := len(arr.items)
			start, end := 0, l
			if len(args) >= 1 && args[0].kind != valUndefined {
				start = int(args[0].toNumber())
				if start < 0 {
					start = max(0, l+start)
				}
				if start > l {
					start = l
				}
			}
			if len(args) >= 2 && args[1].kind != valUndefined {
				end = int(args[1].toNumber())
				if end < 0 {
					end = max(0, l+end)
				}
				if end > l {
					end = l
				}
			}
			if end < start {
				end = start
			}
			cp := make([]jsValue, end-start)
			copy(cp, arr.items[start:end])
			return jsValue{kind: valArray, arr: &jsArray{items: cp}}, nil
		})
	case "indexOf":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 {
				return jsValue{kind: valNumber, num: -1}, nil
			}
			for i, it := range arr.items {
				if err := rt.checkInterrupt(); err != nil {
					return jsUndefinedVal, err
				}
				if jsStrictEqual(it, args[0]) {
					return jsValue{kind: valNumber, num: float64(i)}, nil
				}
			}
			return jsValue{kind: valNumber, num: -1}, nil
		})
	case "includes":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 {
				return jsFalseVal, nil
			}
			for _, it := range arr.items {
				if err := rt.checkInterrupt(); err != nil {
					return jsUndefinedVal, err
				}
				if jsStrictEqual(it, args[0]) {
					return jsTrueVal, nil
				}
			}
			return jsFalseVal, nil
		})
	case "map":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 || args[0].kind != valFunc {
				return jsValue{kind: valArray, arr: &jsArray{}}, nil
			}
			out := make([]jsValue, len(arr.items))
			for i, it := range arr.items {
				if err := rt.checkInterrupt(); err != nil {
					return jsUndefinedVal, err
				}
				v, err := rt.callFunc(args[0].fn, jsUndefinedVal, []jsValue{it, {kind: valNumber, num: float64(i)}})
				if err != nil {
					return jsUndefinedVal, err
				}
				out[i] = v
			}
			return jsValue{kind: valArray, arr: &jsArray{items: out}}, nil
		})
	case "filter":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 || args[0].kind != valFunc {
				return jsValue{kind: valArray, arr: &jsArray{}}, nil
			}
			var out []jsValue
			for i, it := range arr.items {
				if err := rt.checkInterrupt(); err != nil {
					return jsUndefinedVal, err
				}
				v, err := rt.callFunc(args[0].fn, jsUndefinedVal, []jsValue{it, {kind: valNumber, num: float64(i)}})
				if err != nil {
					return jsUndefinedVal, err
				}
				if v.toBool() {
					out = append(out, it)
				}
			}
			return jsValue{kind: valArray, arr: &jsArray{items: out}}, nil
		})
	case "forEach":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 || args[0].kind != valFunc {
				return jsUndefinedVal, nil
			}
			for i, it := range arr.items {
				if err := rt.checkInterrupt(); err != nil {
					return jsUndefinedVal, err
				}
				if _, err := rt.callFunc(args[0].fn, jsUndefinedVal, []jsValue{it, {kind: valNumber, num: float64(i)}}); err != nil {
					return jsUndefinedVal, err
				}
			}
			return jsUndefinedVal, nil
		})
	}
	return jsUndefinedVal
}

func regexpProperty(rx *jsRegExp, prop string) jsValue {
	switch prop {
	case "test":
		return nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			s := ""
			if len(args) > 0 {
				s = args[0].toString()
			}
			return boolVal(rx.re.MatchString(s)), nil
		})
	case "exec":
		return nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			s := ""
			if len(args) > 0 {
				s = args[0].toString()
			}
			sub := rx.re.FindStringSubmatch(s)
			if sub == nil {
				return jsNullVal, nil
			}
			items := make([]jsValue, len(sub))
			for i, m := range sub {
				items[i] = jsValue{kind: valString, str: m}
			}
			return jsValue{kind: valArray, arr: &jsArray{items: items}}, nil
		})
	}
	return jsUndefinedVal
}

func (rt *jsRuntime) installBuiltins() {
	rt.global.define("NaN", jsValue{kind: valNumber, num: math.NaN()}, true)
	rt.global.define("Infinity", jsValue{kind: valNumber, num: math.Inf(1)}, true)

	rt.global.define("String", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return jsValue{kind: valString, str: ""}, nil
		}
		return jsValue{kind: valString, str: args[0].toString()}, nil
	}), false)

	rt.global.define("Number", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return jsValue{kind: valNumber, num: 0}, nil
		}
		return jsValue{kind: valNumber, num: args[0].toNumber()}, nil
	}), false)

	rt.global.define("Boolean", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return jsFalseVal, nil
		}
		return boolVal(args[0].toBool()), nil
	}), false)

	rt.global.define("parseInt", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return jsValue{kind: valNumber, num: math.NaN()}, nil
		}
		base := 10
		if len(args) >= 2 && args[1].kind != valUndefined {
			base = int(args[1].toNumber())
		}
		s := strings.TrimSpace(args[0].toString())
		v, err := strconv.ParseInt(s, base, 64)
		if err != nil {
			return jsValue{kind: valNumber, num: math.NaN()}, nil
		}
		return jsValue{kind: valNumber, num: float64(v)}, nil
	}), false)

	rt.global.define("parseFloat", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return jsValue{kind: valNumber, num: math.NaN()}, nil
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(args[0].toString()), 64)
		if err != nil {
			return jsValue{kind: valNumber, num: math.NaN()}, nil
		}
		return jsValue{kind: valNumber, num: f}, nil
	}), false)

	rt.global.define("isNaN", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return jsTrueVal, nil
		}
		return boolVal(math.IsNaN(args[0].toNumber())), nil
	}), false)

	rt.global.define("isFinite", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return jsFalseVal, nil
		}
		n := args[0].toNumber()
		return boolVal(!math.IsNaN(n) && !math.IsInf(n, 0)), nil
	}), false)

	rt.global.define("encodeURIComponent", nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return jsValue{kind: valString, str: ""}, nil
		}
		escaped := url.QueryEscape(args[0].toString())
		if len(escaped) > maxJSStringBytes {
			return jsUndefinedVal, &jsThrownError{val: r.newErrorVal("RangeError", "Invalid string length")}
		}
		return jsValue{kind: valString, str: escaped}, nil
	}), false)

	rt.global.define("decodeURIComponent", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return jsValue{kind: valString, str: ""}, nil
		}
		u, err := url.QueryUnescape(args[0].toString())
		if err != nil {
			return jsValue{kind: valString, str: args[0].toString()}, nil
		}
		return jsValue{kind: valString, str: u}, nil
	}), false)

	for _, errType := range []string{"Error", "TypeError", "RangeError", "SyntaxError", "ReferenceError"} {
		et := errType
		rt.global.define(et, nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			msg := ""
			if len(args) > 0 && args[0].kind != valUndefined {
				msg = args[0].toString()
			}
			return r.newErrorVal(et, msg), nil
		}), false)
	}

	rt.global.define("RegExp", nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		pat, flags := "", ""
		if len(args) >= 1 {
			pat = args[0].toString()
		}
		if len(args) >= 2 && args[1].kind != valUndefined {
			flags = args[1].toString()
		}
		return r.compileRegExp(pat, flags)
	}), false)

	// JSON
	jsonObj := rt.NewObject()
	jsonObj.setVal("stringify", nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 || args[0].kind == valUndefined {
			return jsUndefinedVal, nil
		}
		b, err := json.Marshal(args[0].Export())
		if err != nil {
			return jsUndefinedVal, &jsThrownError{val: r.newErrorVal("TypeError", err.Error())}
		}
		if len(b) > maxJSStringBytes {
			return jsUndefinedVal, &jsThrownError{val: r.newErrorVal("RangeError", "Invalid string length")}
		}
		return jsValue{kind: valString, str: string(b)}, nil
	}))
	jsonObj.setVal("parse", nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return jsUndefinedVal, &jsThrownError{val: r.newErrorVal("SyntaxError", "Unexpected end of JSON input")}
		}
		var raw any
		if err := json.Unmarshal([]byte(args[0].toString()), &raw); err != nil {
			return jsUndefinedVal, &jsThrownError{val: r.newErrorVal("SyntaxError", err.Error())}
		}
		return r.ToValue(raw), nil
	}))
	rt.global.define("JSON", jsValue{kind: valObject, obj: jsonObj}, false)

	// Math
	mathObj := rt.NewObject()
	mathObj.setVal("PI", jsValue{kind: valNumber, num: math.Pi})
	mathObj.setVal("floor", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return jsValue{kind: valNumber, num: math.NaN()}, nil
		}
		return jsValue{kind: valNumber, num: math.Floor(args[0].toNumber())}, nil
	}))
	mathObj.setVal("ceil", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return jsValue{kind: valNumber, num: math.NaN()}, nil
		}
		return jsValue{kind: valNumber, num: math.Ceil(args[0].toNumber())}, nil
	}))
	mathObj.setVal("round", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return jsValue{kind: valNumber, num: math.NaN()}, nil
		}
		return jsValue{kind: valNumber, num: math.Round(args[0].toNumber())}, nil
	}))
	mathObj.setVal("abs", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return jsValue{kind: valNumber, num: math.NaN()}, nil
		}
		return jsValue{kind: valNumber, num: math.Abs(args[0].toNumber())}, nil
	}))
	mathObj.setVal("random", nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
		return jsValue{kind: valNumber, num: rand.Float64()}, nil
	}))
	mathObj.setVal("max", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return jsValue{kind: valNumber, num: math.Inf(-1)}, nil
		}
		m := args[0].toNumber()
		for _, a := range args[1:] {
			if v := a.toNumber(); v > m {
				m = v
			}
		}
		return jsValue{kind: valNumber, num: m}, nil
	}))
	mathObj.setVal("min", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return jsValue{kind: valNumber, num: math.Inf(1)}, nil
		}
		m := args[0].toNumber()
		for _, a := range args[1:] {
			if v := a.toNumber(); v < m {
				m = v
			}
		}
		return jsValue{kind: valNumber, num: m}, nil
	}))
	rt.global.define("Math", jsValue{kind: valObject, obj: mathObj}, false)

	// Array & Object static helpers
	arrCtor := rt.NewObject()
	arrCtor.setVal("isArray", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		return boolVal(len(args) > 0 && args[0].kind == valArray), nil
	}))
	rt.global.define("Array", jsValue{kind: valObject, obj: arrCtor}, false)

	objCtor := rt.NewObject()
	objCtor.setVal("keys", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		var keys []jsValue
		if len(args) > 0 && args[0].kind == valObject && args[0].obj != nil {
			for k := range args[0].obj.props {
				keys = append(keys, jsValue{kind: valString, str: k})
			}
		}
		return jsValue{kind: valArray, arr: &jsArray{items: keys}}, nil
	}))
	objCtor.setVal("assign", nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return jsValue{kind: valObject, obj: r.NewObject()}, nil
		}
		dst := args[0]
		if dst.kind == valObject && dst.obj != nil {
			for _, src := range args[1:] {
				if src.kind == valObject && src.obj != nil {
					for k, v := range src.obj.props {
						dst.obj.setVal(k, v)
					}
				}
			}
		}
		return dst, nil
	}))
	rt.global.define("Object", jsValue{kind: valObject, obj: objCtor}, false)

	// Web-standard Base64 helpers (btoa / atob)
	rt.global.define("btoa", nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		var s string
		if len(args) > 0 {
			s = args[0].toString()
		}
		buf := make([]byte, 0, len(s))
		for _, rn := range s {
			if rn > 255 {
				return jsUndefinedVal, &jsThrownError{val: r.newErrorVal("InvalidCharacterError", "btoa input contains characters outside Latin1 range")}
			}
			buf = append(buf, byte(rn))
		}
		return jsValue{kind: valString, str: base64.StdEncoding.EncodeToString(buf)}, nil
	}), false)

	rt.global.define("atob", nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		var s string
		if len(args) > 0 {
			s = args[0].toString()
		}
		var clean strings.Builder
		clean.Grow(len(s))
		for i := 0; i < len(s); i++ {
			switch s[i] {
			case ' ', '\t', '\n', '\r', '\f':
				continue
			default:
				clean.WriteByte(s[i])
			}
		}
		encoded := clean.String()
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(encoded, "="))
			if err != nil {
				return jsUndefinedVal, &jsThrownError{val: r.newErrorVal("InvalidCharacterError", "atob input is not valid base64")}
			}
		}
		runes := make([]rune, len(decoded))
		for i, b := range decoded {
			runes[i] = rune(b)
		}
		return jsValue{kind: valString, str: string(runes)}, nil
	}), false)

	// Constructible Date (`new Date()`, `new Date(ms)`, `new Date(isoStr)`, `Date()`, `Date.now()`, `Date.parse()`)
	dateCtor := &jsFunc{
		name: "Date",
		native: func(r *jsRuntime, thisVal jsValue, args []jsValue) (jsValue, error) {
			if thisVal.kind == valUndefined {
				return jsValue{kind: valString, str: time.Now().UTC().Format(time.RFC1123)}, nil
			}
			ms := parseJSDateArgs(args)
			return newJSDateVal(r, ms), nil
		},
		props: map[string]jsValue{
			"now": nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
				return jsValue{kind: valNumber, num: float64(time.Now().UnixMilli())}, nil
			}),
			"parse": nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
				if len(args) == 0 {
					return jsValue{kind: valNumber, num: math.NaN()}, nil
				}
				return jsValue{kind: valNumber, num: parseJSDateString(args[0].toString())}, nil
			}),
		},
	}
	rt.global.define("Date", jsValue{kind: valFunc, fn: dateCtor}, false)
}

func parseJSDateArgs(args []jsValue) float64 {
	if len(args) == 0 {
		return float64(time.Now().UnixMilli())
	}
	if len(args) == 1 {
		a := args[0]
		if a.kind == valObject && a.obj != nil && a.obj.isDate {
			return a.obj.epochMs
		}
		if a.kind == valString {
			return parseJSDateString(a.str)
		}
		n := a.toNumber()
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return math.NaN()
		}
		return math.Trunc(n)
	}
	year := int(args[0].toNumber())
	if year >= 0 && year <= 99 {
		year += 1900
	}
	month := int(args[1].toNumber()) + 1
	day := 1
	if len(args) > 2 {
		day = int(args[2].toNumber())
	}
	var hour, min, sec, msec int
	if len(args) > 3 {
		hour = int(args[3].toNumber())
	}
	if len(args) > 4 {
		min = int(args[4].toNumber())
	}
	if len(args) > 5 {
		sec = int(args[5].toNumber())
	}
	if len(args) > 6 {
		msec = int(args[6].toNumber())
	}
	t := time.Date(year, time.Month(month), day, hour, min, sec, msec*int(time.Millisecond), time.UTC)
	return float64(t.UnixMilli())
}

func parseJSDateString(raw string) float64 {
	s := strings.TrimSpace(raw)
	if s == "" {
		return math.NaN()
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
		time.RFC1123Z,
		time.RFC1123,
		time.RFC822Z,
		time.RFC822,
		time.ANSIC,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return float64(t.UnixMilli())
		}
	}
	return math.NaN()
}

func newJSDateVal(rt *jsRuntime, epochMs float64) jsValue {
	d := &jsObject{
		isDate:  true,
		epochMs: epochMs,
		props:   make(map[string]jsValue, 14),
	}
	validTime := func() (time.Time, bool) {
		if math.IsNaN(d.epochMs) || math.IsInf(d.epochMs, 0) {
			return time.Time{}, false
		}
		return time.UnixMilli(int64(d.epochMs)).UTC(), true
	}
	isoFn := nativeVal(func(r *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
		t, ok := validTime()
		if !ok {
			return jsUndefinedVal, &jsThrownError{val: r.newErrorVal("RangeError", "Invalid time value")}
		}
		return jsValue{kind: valString, str: t.Format("2006-01-02T15:04:05.000Z")}, nil
	})
	d.setVal("toISOString", isoFn)
	d.setVal("toJSON", nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
		t, ok := validTime()
		if !ok {
			return jsNullVal, nil
		}
		return jsValue{kind: valString, str: t.Format("2006-01-02T15:04:05.000Z")}, nil
	}))
	timeFn := nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
		return jsValue{kind: valNumber, num: d.epochMs}, nil
	})
	d.setVal("getTime", timeFn)
	d.setVal("valueOf", timeFn)
	utcStrFn := nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
		t, ok := validTime()
		if !ok {
			return jsValue{kind: valString, str: "Invalid Date"}, nil
		}
		return jsValue{kind: valString, str: t.Format(time.RFC1123)}, nil
	})
	d.setVal("toUTCString", utcStrFn)
	d.setVal("toString", utcStrFn)
	yearFn := nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
		t, ok := validTime()
		if !ok {
			return jsValue{kind: valNumber, num: math.NaN()}, nil
		}
		return jsValue{kind: valNumber, num: float64(t.Year())}, nil
	})
	d.setVal("getFullYear", yearFn)
	d.setVal("getUTCFullYear", yearFn)
	monthFn := nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
		t, ok := validTime()
		if !ok {
			return jsValue{kind: valNumber, num: math.NaN()}, nil
		}
		return jsValue{kind: valNumber, num: float64(t.Month() - 1)}, nil
	})
	d.setVal("getMonth", monthFn)
	d.setVal("getUTCMonth", monthFn)
	dayFn := nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
		t, ok := validTime()
		if !ok {
			return jsValue{kind: valNumber, num: math.NaN()}, nil
		}
		return jsValue{kind: valNumber, num: float64(t.Day())}, nil
	})
	d.setVal("getDate", dayFn)
	d.setVal("getUTCDate", dayFn)
	hourFn := nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
		t, ok := validTime()
		if !ok {
			return jsValue{kind: valNumber, num: math.NaN()}, nil
		}
		return jsValue{kind: valNumber, num: float64(t.Hour())}, nil
	})
	d.setVal("getHours", hourFn)
	d.setVal("getUTCHours", hourFn)
	minFn := nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
		t, ok := validTime()
		if !ok {
			return jsValue{kind: valNumber, num: math.NaN()}, nil
		}
		return jsValue{kind: valNumber, num: float64(t.Minute())}, nil
	})
	d.setVal("getMinutes", minFn)
	d.setVal("getUTCMinutes", minFn)
	secFn := nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
		t, ok := validTime()
		if !ok {
			return jsValue{kind: valNumber, num: math.NaN()}, nil
		}
		return jsValue{kind: valNumber, num: float64(t.Second())}, nil
	})
	d.setVal("getSeconds", secFn)
	d.setVal("getUTCSeconds", secFn)
	_ = rt
	return jsValue{kind: valObject, obj: d}
}

// --- Go <-> JS Marshaling (`json` struct tag aware) ---

func (rt *jsRuntime) ToValue(v any) jsValue {
	return goToJSValue(v)
}

func goToJSValue(v any) jsValue {
	if v == nil {
		return jsNullVal
	}
	switch x := v.(type) {
	case jsValue:
		return x
	case *jsObject:
		if x == nil {
			return jsNullVal
		}
		return jsValue{kind: valObject, obj: x}
	case jsNativeFn:
		return nativeVal(x)
	case bool:
		return boolVal(x)
	case int:
		return jsValue{kind: valNumber, num: float64(x)}
	case int64:
		return jsValue{kind: valNumber, num: float64(x)}
	case float64:
		return jsValue{kind: valNumber, num: x}
	case string:
		return jsValue{kind: valString, str: x}
	case map[string]any:
		o := &jsObject{props: make(map[string]jsValue, len(x))}
		for k, val := range x {
			o.setVal(k, goToJSValue(val))
		}
		return jsValue{kind: valObject, obj: o}
	case map[string]string:
		o := &jsObject{props: make(map[string]jsValue, len(x))}
		for k, val := range x {
			o.setVal(k, jsValue{kind: valString, str: val})
		}
		return jsValue{kind: valObject, obj: o}
	}

	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return jsNullVal
		}
		rv = rv.Elem()
	}

	switch rv.Kind() {
	case reflect.Struct:
		o := &jsObject{props: make(map[string]jsValue, rv.NumField())}
		rtStruct := rv.Type()
		for i := 0; i < rv.NumField(); i++ {
			sf := rtStruct.Field(i)
			if !sf.IsExported() {
				continue
			}
			key := sf.Name
			if tag := sf.Tag.Get("json"); tag != "" {
				namePart := strings.Split(tag, ",")[0]
				if namePart == "-" {
					continue
				}
				if namePart != "" {
					key = namePart
				}
			}
			o.setVal(key, goToJSValue(rv.Field(i).Interface()))
		}
		return jsValue{kind: valObject, obj: o}
	case reflect.Slice, reflect.Array:
		items := make([]jsValue, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			items[i] = goToJSValue(rv.Index(i).Interface())
		}
		return jsValue{kind: valArray, arr: &jsArray{items: items}}
	case reflect.Map:
		o := &jsObject{props: make(map[string]jsValue, rv.Len())}
		iter := rv.MapRange()
		for iter.Next() {
			o.setVal(fmt.Sprint(iter.Key().Interface()), goToJSValue(iter.Value().Interface()))
		}
		return jsValue{kind: valObject, obj: o}
	case reflect.Func:
		return wrapGoFuncVal(rv)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return jsValue{kind: valNumber, num: float64(rv.Int())}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return jsValue{kind: valNumber, num: float64(rv.Uint())}
	case reflect.Float32, reflect.Float64:
		return jsValue{kind: valNumber, num: rv.Float()}
	case reflect.Bool:
		return boolVal(rv.Bool())
	case reflect.String:
		return jsValue{kind: valString, str: rv.String()}
	}
	return jsValue{kind: valString, str: fmt.Sprint(v)}
}

func (rt *jsRuntime) wrapGoFunc(fnVal reflect.Value) jsValue {
	return wrapGoFuncVal(fnVal)
}

func wrapGoFuncVal(fnVal reflect.Value) jsValue {
	fnType := fnVal.Type()
	return nativeVal(func(callRT *jsRuntime, _ jsValue, args []jsValue) (ret jsValue, retErr error) {
		defer func() {
			if rec := recover(); rec != nil {
				if thrown, ok := rec.(*jsThrownError); ok {
					retErr = thrown
				} else {
					retErr = &jsThrownError{val: callRT.newErrorVal("Error", fmt.Sprint(rec))}
				}
			}
		}()
		numIn := fnType.NumIn()
		in := make([]reflect.Value, numIn)
		for i := 0; i < numIn; i++ {
			var av jsValue = jsUndefinedVal
			if i < len(args) {
				av = args[i]
			}
			in[i] = coerceJSArgToReflect(av, fnType.In(i))
		}
		outs := fnVal.Call(in)
		if len(outs) == 0 {
			return jsUndefinedVal, nil
		}
		if len(outs) == 2 && outs[1].Type().Implements(reflect.TypeFor[error]()) {
			if !outs[1].IsNil() {
				errVal := outs[1].Interface().(error)
				return jsUndefinedVal, &jsThrownError{val: callRT.newErrorVal("Error", errVal.Error())}
			}
		}
		return goToJSValue(outs[0].Interface()), nil
	})
}

func coerceJSArgToReflect(v jsValue, target reflect.Type) reflect.Value {
	if v.kind == valUndefined || v.kind == valNull {
		return reflect.Zero(target)
	}
	switch target.Kind() {
	case reflect.String:
		return reflect.ValueOf(v.toString()).Convert(target)
	case reflect.Int:
		return reflect.ValueOf(int(v.toNumber())).Convert(target)
	case reflect.Int64:
		return reflect.ValueOf(int64(v.toNumber())).Convert(target)
	case reflect.Float64:
		return reflect.ValueOf(v.toNumber()).Convert(target)
	case reflect.Bool:
		return reflect.ValueOf(v.toBool()).Convert(target)
	case reflect.Map:
		exp := v.Export()
		if m, ok := exp.(map[string]any); ok && target == reflect.TypeFor[map[string]any]() {
			return reflect.ValueOf(m)
		}
		return reflect.Zero(target)
	case reflect.Interface:
		exp := v.Export()
		if exp == nil {
			return reflect.Zero(target)
		}
		return reflect.ValueOf(exp)
	}
	return reflect.Zero(target)
}

func (v jsValue) Export() any {
	switch v.kind {
	case valUndefined, valNull:
		return nil
	case valBool:
		return v.b
	case valNumber:
		if !math.IsNaN(v.num) && !math.IsInf(v.num, 0) && v.num == math.Trunc(v.num) && v.num >= -9007199254740991 && v.num <= 9007199254740991 {
			return int64(v.num)
		}
		return v.num
	case valString:
		return v.str
	case valArray:
		if v.arr == nil {
			return []any{}
		}
		out := make([]any, len(v.arr.items))
		for i, it := range v.arr.items {
			out[i] = it.Export()
		}
		return out
	case valObject:
		if v.obj == nil {
			return map[string]any{}
		}
		if v.obj.isDate {
			if math.IsNaN(v.obj.epochMs) || math.IsInf(v.obj.epochMs, 0) {
				return nil
			}
			return time.UnixMilli(int64(v.obj.epochMs)).UTC().Format("2006-01-02T15:04:05.000Z")
		}
		out := make(map[string]any, len(v.obj.props))
		for k, pv := range v.obj.props {
			if pv.kind != valUndefined && pv.kind != valFunc {
				out[k] = pv.Export()
			}
		}
		return out
	default:
		return nil
	}
}
