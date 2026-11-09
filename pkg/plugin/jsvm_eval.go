package plugin

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf16"
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

	maxJSArrayLen     = 65536
	maxJSObjectKeys   = 16384
	maxJSStringBytes  = 4 << 20 // 4 MiB string byte-length cap
	maxJSExportDepth  = 64      // Maximum object/array recursion depth
	maxJSExportNodes  = 50_000  // Maximum visited nodes during export/stringify/toString
	maxJSONParseDepth = 64      // Maximum JSON.parse / goToJSValue recursion depth
	maxJSONParseNodes = 100_000 // Maximum JSON.parse / goToJSValue total node budget
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
	scope   *jsScope // non-nil for globalThis proxy object
	isError bool
	isDate  bool
	frozen  bool
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
	name        string
	params      []jsParam
	body        []jsStmt
	env         *jsScope
	native      jsNativeFn
	props       map[string]jsValue
	frozenProps bool
	arrow       bool
	hasBound    bool
	boundThis   jsValue
	boundArgs   []jsValue
}

type jsScope struct {
	parent     *jsScope
	vars       map[string]jsValue
	consts     map[string]bool
	isFuncRoot bool
}

func newJSScope(parent *jsScope) *jsScope {
	return &jsScope{
		parent: parent,
		vars:   make(map[string]jsValue),
	}
}

func newJSFuncScope(parent *jsScope) *jsScope {
	return &jsScope{
		parent:     parent,
		vars:       make(map[string]jsValue),
		isFuncRoot: true,
	}
}

func (s *jsScope) funcRoot() *jsScope {
	cur := s
	for cur.parent != nil && !cur.isFuncRoot {
		cur = cur.parent
	}
	return cur
}

func (s *jsScope) clone() *jsScope {
	cp := &jsScope{
		parent: s.parent,
		vars:   make(map[string]jsValue, len(s.vars)),
	}
	for k, v := range s.vars {
		cp.vars[k] = v
	}
	if len(s.consts) > 0 {
		cp.consts = make(map[string]bool, len(s.consts))
		for k, v := range s.consts {
			cp.consts[k] = v
		}
	}
	return cp
}

func (s *jsScope) define(name string, v jsValue, isConst bool) {
	s.vars[name] = v
	if isConst {
		if s.consts == nil {
			s.consts = make(map[string]bool)
		}
		s.consts[name] = true
	} else if s.consts != nil {
		delete(s.consts, name)
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
	if _, exists := root.vars[name]; !exists && len(root.vars) >= maxJSObjectKeys {
		panic(errJSObjectKeysLimit)
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

func (e *jsInterruptedError) Unwrap() error {
	if err, ok := e.cause.(error); ok {
		return err
	}
	return nil
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
	global          *jsScope
	globalObj       *jsObject
	objProto        *jsObject
	arrProto        *jsObject
	strProto        *jsObject
	numProto        *jsObject
	reProto         *jsObject
	funcProto       *jsObject
	builtinGlobals  map[string]jsValue
	postInitGlobals map[string]jsValue
	postInitConsts  map[string]bool
	protoSnaps      map[*jsObject]map[string]jsValue
	maxStack        int
	stackDepth      int
	interrupted     atomic.Bool
	interruptVal    atomic.Value
}

func newJSRuntime() *jsRuntime {
	rt := &jsRuntime{
		global:   newJSFuncScope(nil),
		maxStack: defaultVMStackLimit,
	}
	rt.installBuiltins()
	return rt
}

// freezeBuiltinsAndSnapshot captures post-module-init global state and freezes
// builtin prototypes/namespaces so pooled VMs cannot leak prototype or globalThis
// mutations across invocations while preserving normal module-level globals.
func (rt *jsRuntime) snapshotPostInit() {
	for _, proto := range []*jsObject{rt.objProto, rt.arrProto, rt.strProto, rt.numProto, rt.reProto, rt.funcProto} {
		if proto != nil {
			proto.frozen = true
		}
	}
	rt.protoSnaps = make(map[*jsObject]map[string]jsValue, 6)
	for _, proto := range []*jsObject{rt.objProto, rt.arrProto, rt.strProto, rt.numProto, rt.reProto, rt.funcProto} {
		if proto != nil {
			cp := make(map[string]jsValue, len(proto.props))
			for k, v := range proto.props {
				cp[k] = v
			}
			rt.protoSnaps[proto] = cp
		}
	}
	for name := range rt.builtinGlobals {
		if v, ok := rt.global.vars[name]; ok {
			if v.kind == valObject && v.obj != nil && v.obj.scope == nil {
				v.obj.frozen = true
			} else if v.kind == valFunc && v.fn != nil {
				v.fn.frozenProps = true
			}
		}
	}
	rt.postInitGlobals = make(map[string]jsValue, len(rt.global.vars))
	for k, v := range rt.global.vars {
		rt.postInitGlobals[k] = v
	}
	if len(rt.global.consts) > 0 {
		rt.postInitConsts = make(map[string]bool, len(rt.global.consts))
		for k, v := range rt.global.consts {
			rt.postInitConsts[k] = v
		}
	}
}

func (rt *jsRuntime) restorePostInit() {
	for proto, snap := range rt.protoSnaps {
		for k := range proto.props {
			if _, ok := snap[k]; !ok {
				delete(proto.props, k)
			}
		}
		for k, v := range snap {
			proto.props[k] = v
		}
		proto.frozen = true
	}
	if rt.postInitGlobals == nil {
		return
	}
	// Remove any undeclared/globalThis properties added during the invocation.
	for k := range rt.global.vars {
		if _, existed := rt.postInitGlobals[k]; !existed {
			delete(rt.global.vars, k)
		}
	}
	// Restore any deleted globals and ensure all builtins match their post-init bindings.
	for k, initVal := range rt.postInitGlobals {
		if _, exists := rt.global.vars[k]; !exists {
			rt.global.vars[k] = initVal
			continue
		}
		if _, isBuiltin := rt.builtinGlobals[k]; isBuiltin {
			rt.global.vars[k] = initVal
		}
	}
	if rt.global.consts != nil {
		for k := range rt.global.consts {
			if !rt.postInitConsts[k] {
				delete(rt.global.consts, k)
			}
		}
	}
	for k, isC := range rt.postInitConsts {
		if isC {
			if rt.global.consts == nil {
				rt.global.consts = make(map[string]bool, len(rt.postInitConsts))
			}
			rt.global.consts[k] = true
		}
	}
}

func (rt *jsRuntime) SetMaxCallStackSize(limit int) {
	if limit > 0 {
		rt.maxStack = limit
	}
}

type jsInterruptBox struct {
	val any
}

func (rt *jsRuntime) Interrupt(v any) {
	if v == nil {
		v = "interrupted"
	}
	rt.interruptVal.Store(jsInterruptBox{val: v})
	rt.interrupted.Store(true)
}

func (rt *jsRuntime) loadInterruptCause() any {
	if box, ok := rt.interruptVal.Load().(jsInterruptBox); ok {
		return box.val
	}
	return nil
}

func (rt *jsRuntime) ClearInterrupt() {
	rt.interrupted.Store(false)
}

func (rt *jsRuntime) checkInterrupt() error {
	if rt.interrupted.Load() {
		return &jsInterruptedError{cause: rt.loadInterruptCause()}
	}
	return nil
}

type jsLimitError string

func (e jsLimitError) Error() string { return string(e) }

const errJSObjectKeysLimit = jsLimitError("RangeError: maximum object keys (16384) exceeded")

func (rt *jsRuntime) NewObject() *jsObject {
	return &jsObject{props: make(map[string]jsValue)}
}

func (o *jsObject) Set(key string, v any) {
	if o.frozen {
		return
	}
	if o.scope != nil {
		_ = o.scope.set(key, goToJSValue(v))
		return
	}
	if o.props == nil {
		o.props = make(map[string]jsValue)
	}
	if _, exists := o.props[key]; !exists && len(o.props) >= maxJSObjectKeys {
		panic(errJSObjectKeysLimit)
	}
	o.props[key] = goToJSValue(v)
}

func (o *jsObject) setVal(key string, v jsValue) {
	if o.frozen {
		return
	}
	if o.scope != nil {
		_ = o.scope.set(key, v)
		return
	}
	if o.props == nil {
		o.props = make(map[string]jsValue)
	}
	if _, exists := o.props[key]; !exists && len(o.props) >= maxJSObjectKeys {
		panic(errJSObjectKeysLimit)
	}
	o.props[key] = v
}

func (o *jsObject) get(key string) jsValue {
	if o == nil {
		return jsUndefinedVal
	}
	if o.scope != nil {
		if v, ok := o.scope.vars[key]; ok {
			return v
		}
	}
	if o.props == nil {
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

func (rt *jsRuntime) RunProgram(prog *jsProgram) (retVal jsValue, retErr error) {
	if err := rt.checkInterrupt(); err != nil {
		return jsUndefinedVal, err
	}
	defer func() {
		if rec := recover(); rec != nil {
			if thrown, ok := rec.(*jsThrownError); ok {
				retVal = jsUndefinedVal
				retErr = thrown
				return
			}
			if limErr, ok := rec.(jsLimitError); ok && rt.stackDepth == 0 {
				retVal = jsUndefinedVal
				retErr = limErr
				return
			}
			panic(rec)
		}
	}()
	rt.hoistVarDecls(prog.stmts, rt.global)
	rt.hoistFuncs(prog.stmts, rt.global)
	res, err := rt.execStmts(prog.stmts, rt.global, jsUndefinedVal)
	if err != nil {
		return jsUndefinedVal, err
	}
	return res.val, nil
}

func collectPatternNames(pat jsPattern, names *[]string) {
	if pat == nil {
		return
	}
	switch p := pat.(type) {
	case *patIdent:
		*names = append(*names, p.name)
	case *patArray:
		for _, el := range p.elems {
			if el.pat != nil {
				collectPatternNames(el.pat, names)
			}
		}
	case *patObject:
		for _, pr := range p.props {
			if pr.target != nil {
				collectPatternNames(pr.target, names)
			}
		}
	}
}

func (rt *jsRuntime) hoistVarDecls(stmts []jsStmt, funcRoot *jsScope) {
	for _, st := range stmts {
		rt.hoistVarStmt(st, funcRoot)
	}
}

func (rt *jsRuntime) hoistVarStmt(st jsStmt, funcRoot *jsScope) {
	if st == nil {
		return
	}
	switch s := st.(type) {
	case *stmtVarDecl:
		if s.kind == tokVar {
			var names []string
			for _, d := range s.decls {
				collectPatternNames(d.pat, &names)
			}
			for _, name := range names {
				if _, exists := funcRoot.vars[name]; !exists {
					funcRoot.define(name, jsUndefinedVal, false)
				}
			}
		}
	case *stmtBlock:
		rt.hoistVarDecls(s.stmts, funcRoot)
	case *stmtIf:
		rt.hoistVarStmt(s.then, funcRoot)
		if s.alt != nil {
			rt.hoistVarStmt(s.alt, funcRoot)
		}
	case *stmtWhile:
		rt.hoistVarStmt(s.body, funcRoot)
	case *stmtDoWhile:
		rt.hoistVarStmt(s.body, funcRoot)
	case *stmtFor:
		if s.init != nil {
			rt.hoistVarStmt(s.init, funcRoot)
		}
		rt.hoistVarStmt(s.body, funcRoot)
	case *stmtForInOf:
		if s.varKind == tokVar && s.pat != nil {
			var names []string
			collectPatternNames(s.pat, &names)
			for _, name := range names {
				if _, exists := funcRoot.vars[name]; !exists {
					funcRoot.define(name, jsUndefinedVal, false)
				}
			}
		}
		rt.hoistVarStmt(s.body, funcRoot)
	case *stmtSwitch:
		for _, cl := range s.cases {
			rt.hoistVarDecls(cl.stmts, funcRoot)
		}
	case *stmtTry:
		rt.hoistVarDecls(s.block, funcRoot)
		rt.hoistVarDecls(s.handler, funcRoot)
		rt.hoistVarDecls(s.finalizer, funcRoot)
	}
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

func (rt *jsRuntime) callFunc(fn *jsFunc, thisVal jsValue, args []jsValue) (retVal jsValue, retErr error) {
	if err := rt.checkInterrupt(); err != nil {
		return jsUndefinedVal, err
	}
	if fn == nil {
		return rt.throwValErr("TypeError", "value is not a function")
	}
	if rt.stackDepth >= rt.maxStack {
		return rt.throwValErr("RangeError", "Maximum call stack size exceeded")
	}
	outermost := rt.stackDepth == 0
	rt.stackDepth++
	defer func() {
		rt.stackDepth--
		if rec := recover(); rec != nil {
			if thrown, ok := rec.(*jsThrownError); ok {
				retVal = jsUndefinedVal
				retErr = thrown
				return
			}
			if limErr, ok := rec.(jsLimitError); ok && outermost {
				retVal = jsUndefinedVal
				retErr = limErr
				return
			}
			panic(rec)
		}
	}()

	if fn.hasBound {
		thisVal = fn.boundThis
	}
	if len(fn.boundArgs) > 0 {
		if len(fn.boundArgs)+len(args) > maxJSArrayLen {
			return rt.throwValErr("RangeError", "Maximum call arguments exceeded")
		}
		combined := make([]jsValue, 0, len(fn.boundArgs)+len(args))
		combined = append(combined, fn.boundArgs...)
		combined = append(combined, args...)
		args = combined
	}

	if fn.native != nil {
		return fn.native(rt, thisVal, args)
	}

	callScope := newJSFuncScope(fn.env)
	if fn.name != "" {
		callScope.define(fn.name, jsValue{kind: valFunc, fn: fn}, false)
	}
	if !fn.arrow {
		argCopy := make([]jsValue, len(args))
		copy(argCopy, args)
		callScope.define("arguments", jsValue{kind: valArray, arr: &jsArray{items: argCopy}}, false)
	}
	for i, p := range fn.params {
		if p.rest {
			var restItems []jsValue
			if i < len(args) {
				restItems = make([]jsValue, len(args)-i)
				copy(restItems, args[i:])
			} else {
				restItems = []jsValue{}
			}
			restVal := jsValue{kind: valArray, arr: &jsArray{items: restItems}}
			if err := rt.bindPattern(p.pat, restVal, callScope, callScope, thisVal, false); err != nil {
				return jsUndefinedVal, err
			}
			break
		}
		argVal := jsUndefinedVal
		if i < len(args) {
			argVal = args[i]
		}
		if argVal.kind == valUndefined && p.defaultVal != nil {
			var err error
			argVal, err = rt.evalExpr(p.defaultVal, callScope, thisVal)
			if err != nil {
				return jsUndefinedVal, err
			}
		}
		if err := rt.bindPattern(p.pat, argVal, callScope, callScope, thisVal, false); err != nil {
			return jsUndefinedVal, err
		}
	}
	rt.hoistVarDecls(fn.body, callScope)
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

func (rt *jsRuntime) spreadIterValues(v jsValue) ([]jsValue, error) {
	if v.kind == valArray && v.arr != nil {
		return v.arr.items, nil
	}
	if v.kind == valString {
		n := utf8.RuneCountInString(v.str)
		if n > maxJSArrayLen {
			return nil, rt.throwErr("RangeError", "Invalid array length")
		}
		items := make([]jsValue, 0, n)
		for _, r := range v.str {
			items = append(items, jsValue{kind: valString, str: string(r)})
		}
		return items, nil
	}
	return nil, rt.throwErr("TypeError", v.toString()+" is not iterable")
}

func (rt *jsRuntime) bindPattern(pat jsPattern, val jsValue, targetScope *jsScope, evalScope *jsScope, thisVal jsValue, isConst bool) error {
	if err := rt.checkInterrupt(); err != nil {
		return err
	}
	switch p := pat.(type) {
	case *patIdent:
		targetScope.define(p.name, val, isConst)
		return nil
	case *patArray:
		items, err := rt.spreadIterValues(val)
		if err != nil {
			return err
		}
		for i, el := range p.elems {
			if el.pat == nil {
				continue
			}
			if el.rest {
				var restItems []jsValue
				if i < len(items) {
					restItems = make([]jsValue, len(items)-i)
					copy(restItems, items[i:])
				} else {
					restItems = []jsValue{}
				}
				restVal := jsValue{kind: valArray, arr: &jsArray{items: restItems}}
				return rt.bindPattern(el.pat, restVal, targetScope, evalScope, thisVal, isConst)
			}
			elemVal := jsUndefinedVal
			if i < len(items) {
				elemVal = items[i]
			}
			if elemVal.kind == valUndefined && el.defaultVal != nil {
				elemVal, err = rt.evalExpr(el.defaultVal, evalScope, thisVal)
				if err != nil {
					return err
				}
			}
			if err := rt.bindPattern(el.pat, elemVal, targetScope, evalScope, thisVal, isConst); err != nil {
				return err
			}
		}
		return nil
	case *patObject:
		if val.kind == valUndefined || val.kind == valNull {
			return rt.throwErr("TypeError", "Cannot destructure property of "+val.toString())
		}
		usedKeys := make(map[string]bool, len(p.props))
		for _, pr := range p.props {
			if pr.rest {
				restObj := rt.NewObject()
				if val.kind == valObject && val.obj != nil {
					for k, pv := range val.obj.props {
						if !usedKeys[k] {
							restObj.setVal(k, pv)
						}
					}
				}
				return rt.bindPattern(pr.target, jsValue{kind: valObject, obj: restObj}, targetScope, evalScope, thisVal, isConst)
			}
			key := pr.key
			if pr.computedKey != nil {
				kv, err := rt.evalExpr(pr.computedKey, evalScope, thisVal)
				if err != nil {
					return err
				}
				key = kv.toString()
			}
			usedKeys[key] = true
			propVal := rt.getProperty(val, key)
			if propVal.kind == valUndefined && pr.defaultVal != nil {
				var err error
				propVal, err = rt.evalExpr(pr.defaultVal, evalScope, thisVal)
				if err != nil {
					return err
				}
			}
			if err := rt.bindPattern(pr.target, propVal, targetScope, evalScope, thisVal, isConst); err != nil {
				return err
			}
		}
		return nil
	}
	return nil
}

func (rt *jsRuntime) assignDestructuring(target jsExpr, val jsValue, scope *jsScope, thisVal jsValue) error {
	if err := rt.checkInterrupt(); err != nil {
		return err
	}
	for {
		par, ok := target.(*exprParen)
		if !ok {
			break
		}
		target = par.inner
	}
	switch t := target.(type) {
	case *exprArray:
		items, err := rt.spreadIterValues(val)
		if err != nil {
			return err
		}
		for i, el := range t.elems {
			if _, isHole := el.(*exprHole); isHole {
				continue
			}
			if sp, isSpread := el.(*exprSpread); isSpread {
				var restItems []jsValue
				if i < len(items) {
					restItems = make([]jsValue, len(items)-i)
					copy(restItems, items[i:])
				} else {
					restItems = []jsValue{}
				}
				return rt.assignDestructuring(sp.arg, jsValue{kind: valArray, arr: &jsArray{items: restItems}}, scope, thisVal)
			}
			elemTarget := el
			var defExpr jsExpr
			if asg, ok := el.(*exprAssign); ok && asg.op == tokAssign {
				elemTarget = asg.left
				defExpr = asg.right
			}
			elemVal := jsUndefinedVal
			if i < len(items) {
				elemVal = items[i]
			}
			if elemVal.kind == valUndefined && defExpr != nil {
				elemVal, err = rt.evalExpr(defExpr, scope, thisVal)
				if err != nil {
					return err
				}
			}
			if err := rt.assignDestructuring(elemTarget, elemVal, scope, thisVal); err != nil {
				return err
			}
		}
		return nil
	case *exprObject:
		if val.kind == valUndefined || val.kind == valNull {
			return rt.throwErr("TypeError", "Cannot destructure property of "+val.toString())
		}
		usedKeys := make(map[string]bool, len(t.props))
		for _, pr := range t.props {
			if pr.spread {
				restObj := rt.NewObject()
				if val.kind == valObject && val.obj != nil {
					for k, pv := range val.obj.props {
						if !usedKeys[k] {
							restObj.setVal(k, pv)
						}
					}
				}
				return rt.assignDestructuring(pr.val, jsValue{kind: valObject, obj: restObj}, scope, thisVal)
			}
			key := pr.key
			if pr.computedKey != nil {
				kv, err := rt.evalExpr(pr.computedKey, scope, thisVal)
				if err != nil {
					return err
				}
				key = kv.toString()
			}
			usedKeys[key] = true
			propTarget := pr.val
			defExpr := pr.defaultVal
			if asg, ok := pr.val.(*exprAssign); ok && asg.op == tokAssign && defExpr == nil {
				propTarget = asg.left
				defExpr = asg.right
			}
			propVal := rt.getProperty(val, key)
			if propVal.kind == valUndefined && defExpr != nil {
				var err error
				propVal, err = rt.evalExpr(defExpr, scope, thisVal)
				if err != nil {
					return err
				}
			}
			if err := rt.assignDestructuring(propTarget, propVal, scope, thisVal); err != nil {
				return err
			}
		}
		return nil
	default:
		_, setter, err := rt.resolveLValue(target, scope, thisVal)
		if err != nil {
			return err
		}
		if err := setter(val); err != nil {
			var thrown *jsThrownError
			var inter *jsInterruptedError
			if errors.As(err, &thrown) || errors.As(err, &inter) {
				return err
			}
			return rt.throwErr("TypeError", err.Error())
		}
		return nil
	}
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
		targetScope := scope
		if s.kind == tokVar {
			targetScope = scope.funcRoot()
		}
		for _, d := range s.decls {
			if s.kind == tokVar && d.init == nil {
				if id, ok := d.pat.(*patIdent); ok {
					if _, exists := targetScope.vars[id.name]; !exists {
						targetScope.define(id.name, jsUndefinedVal, false)
					}
					continue
				}
			}
			val := jsUndefinedVal
			if d.init != nil {
				var err error
				val, err = rt.evalExpr(d.init, scope, thisVal)
				if err != nil {
					return flowResult{}, err
				}
			}
			if err := rt.bindPattern(d.pat, val, targetScope, scope, thisVal, s.kind == tokConst); err != nil {
				return flowResult{}, err
			}
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
		iterScope := newJSScope(scope)
		perIterClone := false
		if vd, ok := s.init.(*stmtVarDecl); ok && (vd.kind == tokLet || vd.kind == tokConst) {
			perIterClone = true
		}
		if s.init != nil {
			if _, err := rt.execStmt(s.init, iterScope, thisVal); err != nil {
				return flowResult{}, err
			}
		}
		if perIterClone {
			iterScope = iterScope.clone()
		}
		for {
			if err := rt.checkInterrupt(); err != nil {
				return flowResult{}, err
			}
			if s.cond != nil {
				c, err := rt.evalExpr(s.cond, iterScope, thisVal)
				if err != nil || !c.toBool() {
					return flowResult{}, err
				}
			}
			res, err := rt.execStmt(s.body, iterScope, thisVal)
			if err != nil {
				return flowResult{}, err
			}
			if res.kind == flowBreak {
				return flowResult{}, nil
			}
			if res.kind == flowReturn {
				return res, nil
			}
			if perIterClone {
				iterScope = iterScope.clone()
			}
			if s.post != nil {
				if _, err := rt.evalExpr(s.post, iterScope, thisVal); err != nil {
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
			if target.kind == valUndefined || target.kind == valNull {
				return flowResult{}, rt.throwErr("TypeError", target.toString()+" is not iterable")
			}
			items, err := rt.spreadIterValues(target)
			if err != nil {
				return flowResult{}, err
			}
			iterVals = append(iterVals, items...)
		} else {
			if target.kind == valObject && target.obj != nil {
				for _, k := range sortedObjectKeys(target.obj) {
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
			switch s.varKind {
			case tokVar:
				if err := rt.bindPattern(s.pat, iv, scope.funcRoot(), iterScope, thisVal, false); err != nil {
					return flowResult{}, err
				}
			case tokLet, tokConst:
				if err := rt.bindPattern(s.pat, iv, iterScope, iterScope, thisVal, s.varKind == tokConst); err != nil {
					return flowResult{}, err
				}
			default:
				if err := rt.assignDestructuring(s.lhs, iv, iterScope, thisVal); err != nil {
					return flowResult{}, err
				}
			}
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
		switchScope := newJSScope(scope)
		for _, cl := range s.cases {
			rt.hoistFuncs(cl.stmts, switchScope)
		}
		startIdx := -1
		defaultIdx := -1
		for idx, cl := range s.cases {
			if cl.test == nil {
				defaultIdx = idx
				continue
			}
			tv, err := rt.evalExpr(cl.test, switchScope, thisVal)
			if err != nil {
				return flowResult{}, err
			}
			if jsStrictEqual(disc, tv) {
				startIdx = idx
				break
			}
		}
		if startIdx < 0 {
			startIdx = defaultIdx
		}
		if startIdx >= 0 {
			for idx := startIdx; idx < len(s.cases); idx++ {
				res, err := rt.execStmts(s.cases[idx].stmts, switchScope, thisVal)
				if err != nil {
					return flowResult{}, err
				}
				if res.kind == flowBreak {
					return flowResult{}, nil
				}
				if res.kind == flowReturn || res.kind == flowContinue {
					return res, nil
				}
			}
		}
		return flowResult{}, nil
	case *stmtTry:
		tryScope := newJSScope(scope)
		rt.hoistFuncs(s.block, tryScope)
		res, err := rt.execStmts(s.block, tryScope, thisVal)
		var inter *jsInterruptedError
		if errors.As(err, &inter) {
			return flowResult{}, err
		}
		if err != nil && s.hasCatch {
			catchScope := newJSScope(scope)
			if s.catchPat != nil {
				var catchVal jsValue
				var thrown *jsThrownError
				if errors.As(err, &thrown) {
					catchVal = thrown.val
				} else {
					catchVal = rt.newErrorVal("Error", err.Error())
				}
				if bErr := rt.bindPattern(s.catchPat, catchVal, catchScope, catchScope, thisVal, false); bErr != nil {
					err = bErr
				} else {
					rt.hoistFuncs(s.handler, catchScope)
					res, err = rt.execStmts(s.handler, catchScope, thisVal)
				}
			} else {
				rt.hoistFuncs(s.handler, catchScope)
				res, err = rt.execStmts(s.handler, catchScope, thisVal)
			}
		}
		if len(s.finalizer) > 0 {
			finScope := newJSScope(scope)
			rt.hoistFuncs(s.finalizer, finScope)
			fRes, fErr := rt.execStmts(s.finalizer, finScope, thisVal)
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

func hasOptionalLink(ex jsExpr) bool {
	switch e := ex.(type) {
	case *exprMember:
		return e.optional || hasOptionalLink(e.obj)
	case *exprCall:
		if e.isNew {
			return false
		}
		return e.optional || hasOptionalLink(e.callee)
	default:
		return false
	}
}

func (rt *jsRuntime) evalCallArgs(rawArgs []jsExpr, scope *jsScope, thisVal jsValue) ([]jsValue, error) {
	argVals := make([]jsValue, 0, len(rawArgs))
	for _, a := range rawArgs {
		if sp, ok := a.(*exprSpread); ok {
			sv, err := rt.evalExpr(sp.arg, scope, thisVal)
			if err != nil {
				return nil, err
			}
			items, err := rt.spreadIterValues(sv)
			if err != nil {
				return nil, err
			}
			if len(argVals)+len(items) > maxJSArrayLen {
				return nil, rt.throwErr("RangeError", "Maximum call arguments exceeded")
			}
			argVals = append(argVals, items...)
			continue
		}
		av, err := rt.evalExpr(a, scope, thisVal)
		if err != nil {
			return nil, err
		}
		if len(argVals)+1 > maxJSArrayLen {
			return nil, rt.throwErr("RangeError", "Maximum call arguments exceeded")
		}
		argVals = append(argVals, av)
	}
	return argVals, nil
}

func (rt *jsRuntime) evalChain(ex jsExpr, scope *jsScope, thisVal jsValue) (jsValue, jsValue, bool, error) {
	if err := rt.checkInterrupt(); err != nil {
		return jsUndefinedVal, jsUndefinedVal, false, err
	}
	switch e := ex.(type) {
	case *exprMember:
		var objVal jsValue
		var shortCircuited bool
		var err error
		if hasOptionalLink(e.obj) {
			objVal, _, shortCircuited, err = rt.evalChain(e.obj, scope, thisVal)
			if err != nil || shortCircuited {
				return jsUndefinedVal, jsUndefinedVal, shortCircuited, err
			}
		} else {
			objVal, err = rt.evalExpr(e.obj, scope, thisVal)
			if err != nil {
				return jsUndefinedVal, jsUndefinedVal, false, err
			}
		}
		if objVal.kind == valUndefined || objVal.kind == valNull {
			if e.optional {
				return jsUndefinedVal, jsUndefinedVal, true, nil
			}
			return jsUndefinedVal, jsUndefinedVal, false, rt.throwErr("TypeError", "Cannot read properties of "+objVal.toString())
		}
		prop := e.prop
		if e.computed {
			kv, err := rt.evalExpr(e.index, scope, thisVal)
			if err != nil {
				return jsUndefinedVal, jsUndefinedVal, false, err
			}
			prop = kv.toString()
		}
		return rt.getProperty(objVal, prop), objVal, false, nil

	case *exprCall:
		var calleeVal jsValue
		var receiver = jsUndefinedVal
		var shortCircuited bool
		var err error
		if mem, ok := e.callee.(*exprMember); ok {
			calleeVal, receiver, shortCircuited, err = rt.evalChain(mem, scope, thisVal)
			if err != nil || shortCircuited {
				return jsUndefinedVal, jsUndefinedVal, shortCircuited, err
			}
		} else if hasOptionalLink(e.callee) {
			calleeVal, _, shortCircuited, err = rt.evalChain(e.callee, scope, thisVal)
			if err != nil || shortCircuited {
				return jsUndefinedVal, jsUndefinedVal, shortCircuited, err
			}
		} else {
			calleeVal, err = rt.evalExpr(e.callee, scope, thisVal)
			if err != nil {
				return jsUndefinedVal, jsUndefinedVal, false, err
			}
		}
		if e.optional && (calleeVal.kind == valUndefined || calleeVal.kind == valNull) {
			return jsUndefinedVal, jsUndefinedVal, true, nil
		}
		if calleeVal.kind != valFunc || calleeVal.fn == nil {
			return jsUndefinedVal, jsUndefinedVal, false, rt.throwErr("TypeError", "value is not a function")
		}
		argVals, err := rt.evalCallArgs(e.args, scope, thisVal)
		if err != nil {
			return jsUndefinedVal, jsUndefinedVal, false, err
		}
		res, err := rt.callFunc(calleeVal.fn, receiver, argVals)
		return res, jsUndefinedVal, false, err
	}
	v, err := rt.evalExpr(ex, scope, thisVal)
	return v, jsUndefinedVal, false, err
}

func (rt *jsRuntime) evalExpr(ex jsExpr, scope *jsScope, thisVal jsValue) (jsValue, error) {
	if err := rt.checkInterrupt(); err != nil {
		return jsUndefinedVal, err
	}
	switch e := ex.(type) {
	case *exprParen:
		return rt.evalExpr(e.inner, scope, thisVal)
	case *exprUndefined, *exprHole:
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
			return rt.throwValErr("ReferenceError", e.name+" is not defined")
		}
		return v, nil
	case *exprArray:
		if len(e.elems) > maxJSArrayLen {
			return rt.throwValErr("RangeError", "Invalid array length")
		}
		items := make([]jsValue, 0, len(e.elems))
		for _, el := range e.elems {
			if _, isHole := el.(*exprHole); isHole {
				if len(items)+1 > maxJSArrayLen {
					return rt.throwValErr("RangeError", "Invalid array length")
				}
				items = append(items, jsUndefinedVal)
				continue
			}
			if sp, isSpread := el.(*exprSpread); isSpread {
				sv, err := rt.evalExpr(sp.arg, scope, thisVal)
				if err != nil {
					return jsUndefinedVal, err
				}
				spItems, err := rt.spreadIterValues(sv)
				if err != nil {
					return jsUndefinedVal, err
				}
				if len(items)+len(spItems) > maxJSArrayLen {
					return rt.throwValErr("RangeError", "Invalid array length")
				}
				items = append(items, spItems...)
				continue
			}
			v, err := rt.evalExpr(el, scope, thisVal)
			if err != nil {
				return jsUndefinedVal, err
			}
			if len(items)+1 > maxJSArrayLen {
				return rt.throwValErr("RangeError", "Invalid array length")
			}
			items = append(items, v)
		}
		return jsValue{kind: valArray, arr: &jsArray{items: items}}, nil
	case *exprObject:
		obj := rt.NewObject()
		for _, pr := range e.props {
			if pr.defaultVal != nil {
				return rt.throwValErr("SyntaxError", "Invalid shorthand property initializer")
			}
			if pr.spread {
				sv, err := rt.evalExpr(pr.val, scope, thisVal)
				if err != nil {
					return jsUndefinedVal, err
				}
				if sv.kind == valObject && sv.obj != nil {
					for k, pv := range sv.obj.props {
						obj.setVal(k, pv)
					}
				} else if sv.kind == valArray && sv.arr != nil {
					for idx, pv := range sv.arr.items {
						obj.setVal(strconv.Itoa(idx), pv)
					}
				}
				continue
			}
			key := pr.key
			if pr.computedKey != nil {
				kv, err := rt.evalExpr(pr.computedKey, scope, thisVal)
				if err != nil {
					return jsUndefinedVal, err
				}
				key = kv.toString()
			}
			v, err := rt.evalExpr(pr.val, scope, thisVal)
			if err != nil {
				return jsUndefinedVal, err
			}
			obj.setVal(key, v)
		}
		return jsValue{kind: valObject, obj: obj}, nil
	case *exprFunc:
		fn := &jsFunc{
			name:   e.name,
			params: e.params,
			body:   e.body,
			env:    scope,
			arrow:  e.arrow,
		}
		if e.arrow {
			fn.hasBound = true
			fn.boundThis = thisVal
		}
		return jsValue{kind: valFunc, fn: fn}, nil
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
		argExpr := e.arg
		for {
			par, ok := argExpr.(*exprParen)
			if !ok {
				break
			}
			argExpr = par.inner
		}
		if e.op == tokTypeof {
			if id, ok := argExpr.(*exprIdent); ok {
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
			if mem, ok := argExpr.(*exprMember); ok {
				var objVal jsValue
				var shortCircuited bool
				var err error
				if hasOptionalLink(mem.obj) {
					objVal, _, shortCircuited, err = rt.evalChain(mem.obj, scope, thisVal)
					if err != nil {
						return jsUndefinedVal, err
					}
					if shortCircuited {
						return jsTrueVal, nil
					}
				} else {
					objVal, err = rt.evalExpr(mem.obj, scope, thisVal)
					if err != nil {
						return jsUndefinedVal, err
					}
				}
				if objVal.kind == valUndefined || objVal.kind == valNull {
					if mem.optional {
						return jsTrueVal, nil
					}
					return rt.throwValErr("TypeError", "Cannot convert undefined or null to object")
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
					if objVal.obj.frozen {
						return jsFalseVal, nil
					}
					if objVal.obj.scope != nil {
						if objVal.obj.scope.consts != nil && objVal.obj.scope.consts[key] {
							return jsFalseVal, nil
						}
						delete(objVal.obj.scope.vars, key)
					} else {
						delete(objVal.obj.props, key)
					}
				} else if objVal.kind == valFunc && objVal.fn != nil {
					if objVal.fn.frozenProps {
						return jsFalseVal, nil
					}
					if objVal.fn.props != nil {
						delete(objVal.fn.props, key)
					}
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
			return rt.throwValErr("TypeError", err.Error())
		}
		if e.prefix {
			return nextVal, nil
		}
		return jsValue{kind: valNumber, num: oldNum}, nil
	case *exprAssign:
		if e.op == tokAssign {
			switch e.left.(type) {
			case *exprArray, *exprObject:
				rhs, err := rt.evalExpr(e.right, scope, thisVal)
				if err != nil {
					return jsUndefinedVal, err
				}
				if err := rt.assignDestructuring(e.left, rhs, scope, thisVal); err != nil {
					return jsUndefinedVal, err
				}
				return rhs, nil
			}
		}
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
				ls, lok := cur.toStringChecked()
				if !lok {
					return rt.throwStrLenErr()
				}
				rs, rok := rhs.toStringChecked()
				if !rok || len(ls)+len(rs) > maxJSStringBytes {
					return rt.throwStrLenErr()
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
			return rt.throwValErr("TypeError", err.Error())
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
		val, _, _, err := rt.evalChain(e, scope, thisVal)
		return val, err
	case *exprCall:
		if e.isNew {
			calleeVal, err := rt.evalExpr(e.callee, scope, thisVal)
			if err != nil {
				return jsUndefinedVal, err
			}
			if calleeVal.kind != valFunc || calleeVal.fn == nil || calleeVal.fn.arrow {
				return rt.throwValErr("TypeError", calleeVal.toString()+" is not a constructor")
			}
			argVals, err := rt.evalCallArgs(e.args, scope, thisVal)
			if err != nil {
				return jsUndefinedVal, err
			}
			newThis := jsValue{kind: valObject, obj: rt.NewObject()}
			targetFn := calleeVal.fn
			if targetFn.hasBound && !targetFn.arrow {
				// `new` overrides bound `this` while preserving bound arguments.
				unboundCopy := *targetFn
				unboundCopy.hasBound = false
				targetFn = &unboundCopy
			}
			res, err := rt.callFunc(targetFn, newThis, argVals)
			if err != nil {
				return jsUndefinedVal, err
			}
			if res.kind == valObject || res.kind == valArray || res.kind == valRegExp {
				return res, nil
			}
			return newThis, nil
		}
		val, _, _, err := rt.evalChain(e, scope, thisVal)
		return val, err
	}
	return jsUndefinedVal, nil
}

func (rt *jsRuntime) resolveLValue(target jsExpr, scope *jsScope, thisVal jsValue) (jsValue, func(jsValue) error, error) {
	for {
		par, ok := target.(*exprParen)
		if !ok {
			break
		}
		target = par.inner
	}
	switch lv := target.(type) {
	case *exprIdent:
		cur, _ := scope.get(lv.name)
		return cur, func(v jsValue) error { return scope.set(lv.name, v) }, nil
	case *exprMember:
		if lv.optional || hasOptionalLink(lv.obj) {
			return jsUndefinedVal, nil, rt.throwErr("SyntaxError", "Invalid left-hand side in assignment")
		}
		objVal, err := rt.evalExpr(lv.obj, scope, thisVal)
		if err != nil {
			return jsUndefinedVal, nil, err
		}
		if objVal.kind == valUndefined || objVal.kind == valNull {
			return jsUndefinedVal, nil, rt.throwErr("TypeError", "Cannot set properties of "+objVal.toString())
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
				if objVal.obj.frozen {
					return nil
				}
				if objVal.obj.scope != nil {
					return objVal.obj.scope.set(prop, v)
				}
				objVal.obj.setVal(prop, v)
				return nil
			}
			if objVal.kind == valFunc && objVal.fn != nil {
				if objVal.fn.frozenProps {
					return nil
				}
				if objVal.fn.props == nil {
					objVal.fn.props = make(map[string]jsValue)
				}
				if _, exists := objVal.fn.props[prop]; !exists && len(objVal.fn.props) >= maxJSObjectKeys {
					panic(errJSObjectKeysLimit)
				}
				objVal.fn.props[prop] = v
				return nil
			}
			if objVal.kind == valArray && objVal.arr != nil {
				if idx, pErr := strconv.Atoi(prop); pErr == nil && idx >= 0 {
					if idx >= maxJSArrayLen {
						return rt.throwErr("RangeError", "Invalid array length")
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
				return nil
			}
			return errors.New("Cannot set property " + prop)
		}, nil
	default:
		return jsUndefinedVal, nil, rt.throwErr("ReferenceError", "Invalid left-hand side in assignment")
	}
}

func (rt *jsRuntime) evalBinaryOp(op jsTokKind, l, r jsValue) (jsValue, error) {
	switch op {
	case tokPlus:
		if l.kind == valString || r.kind == valString {
			ls, lok := l.toStringChecked()
			if !lok {
				return rt.throwStrLenErr()
			}
			rs, rok := r.toStringChecked()
			if !rok || len(ls)+len(rs) > maxJSStringBytes {
				return rt.throwStrLenErr()
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
			k := l.toString()
			if r.obj.scope != nil {
				_, ok := r.obj.scope.vars[k]
				return boolVal(ok), nil
			}
			_, ok := r.obj.props[k]
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

func appendBoundedString(b *strings.Builder, s string) bool {
	if b.Len()+len(s) > maxJSStringBytes {
		return false
	}
	b.WriteString(s)
	return true
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
	}
	var b strings.Builder
	nodes := 0
	_ = v.writeToString(&b, nil, nil, 0, &nodes)
	return b.String()
}

func (v jsValue) toStringChecked() (string, bool) {
	switch v.kind {
	case valUndefined, valNull, valBool, valNumber:
		return v.toString(), true
	case valString:
		if len(v.str) > maxJSStringBytes {
			return "", false
		}
		return v.str, true
	}
	var b strings.Builder
	nodes := 0
	if !v.writeToString(&b, nil, nil, 0, &nodes) {
		return "", false
	}
	return b.String(), true
}

func (v jsValue) writeToString(b *strings.Builder, seenArr map[*jsArray]bool, seenObj map[*jsObject]bool, depth int, nodes *int) bool {
	*nodes++
	if *nodes > maxJSExportNodes || depth > maxJSExportDepth {
		return false
	}
	switch v.kind {
	case valUndefined:
		return appendBoundedString(b, "undefined")
	case valNull:
		return appendBoundedString(b, "null")
	case valBool:
		if v.b {
			return appendBoundedString(b, "true")
		}
		return appendBoundedString(b, "false")
	case valNumber:
		return appendBoundedString(b, v.toString())
	case valString:
		return appendBoundedString(b, v.str)
	case valArray:
		if v.arr == nil || len(v.arr.items) == 0 {
			return true
		}
		if seenArr != nil && seenArr[v.arr] {
			// ECMAScript Array.prototype.join / toString breaks active cycles with empty string.
			return true
		}
		if seenArr == nil {
			seenArr = make(map[*jsArray]bool, 4)
		}
		seenArr[v.arr] = true
		defer delete(seenArr, v.arr)
		for i, it := range v.arr.items {
			if i > 0 {
				if !appendBoundedString(b, ",") {
					return false
				}
			}
			if it.kind != valUndefined && it.kind != valNull {
				if !it.writeToString(b, seenArr, seenObj, depth+1, nodes) {
					return false
				}
			}
		}
		return true
	case valObject:
		if v.obj != nil {
			if v.obj.isError {
				if seenObj != nil && seenObj[v.obj] {
					return appendBoundedString(b, "Error")
				}
				if seenObj == nil {
					seenObj = make(map[*jsObject]bool, 2)
				}
				seenObj[v.obj] = true
				defer delete(seenObj, v.obj)
				var nb, mb strings.Builder
				if !v.obj.get("name").writeToString(&nb, seenArr, seenObj, depth+1, nodes) {
					return false
				}
				if !v.obj.get("message").writeToString(&mb, seenArr, seenObj, depth+1, nodes) {
					return false
				}
				name := nb.String()
				msg := mb.String()
				if msg != "" {
					if !appendBoundedString(b, name) || !appendBoundedString(b, ": ") || !appendBoundedString(b, msg) {
						return false
					}
					return true
				}
				return appendBoundedString(b, name)
			}
			if v.obj.isDate {
				if math.IsNaN(v.obj.epochMs) || math.IsInf(v.obj.epochMs, 0) {
					return appendBoundedString(b, "Invalid Date")
				}
				return appendBoundedString(b, time.UnixMilli(int64(v.obj.epochMs)).UTC().Format(time.RFC1123))
			}
		}
		return appendBoundedString(b, "[object Object]")
	default:
		return appendBoundedString(b, "[object]")
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

func (rt *jsRuntime) throwErr(name, msg string) error {
	return &jsThrownError{val: rt.newErrorVal(name, msg)}
}

func (rt *jsRuntime) throwValErr(name, msg string) (jsValue, error) {
	return jsUndefinedVal, rt.throwErr(name, msg)
}

func (rt *jsRuntime) throwStrLenErr() (jsValue, error) {
	return rt.throwValErr("RangeError", "Invalid string length")
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
		return rt.throwValErr("SyntaxError", "Invalid RegExp: "+err.Error())
	}
	return jsValue{
		kind: valRegExp,
		re:   &jsRegExp{pattern: pattern, flags: flags, global: global, re: re},
	}, nil
}

func hasOwnPropValue(target jsValue, prop string) bool {
	switch target.kind {
	case valObject:
		if target.obj == nil {
			return false
		}
		if target.obj.scope != nil {
			_, ok := target.obj.scope.vars[prop]
			return ok
		}
		if target.obj.props == nil {
			return false
		}
		_, ok := target.obj.props[prop]
		return ok
	case valArray:
		if target.arr == nil {
			return false
		}
		if prop == "length" {
			return true
		}
		idx, err := strconv.Atoi(prop)
		return err == nil && idx >= 0 && idx < len(target.arr.items)
	case valString:
		if prop == "length" {
			return true
		}
		idx, err := strconv.Atoi(prop)
		return err == nil && idx >= 0 && idx < utf8.RuneCountInString(target.str)
	case valFunc:
		if target.fn == nil {
			return false
		}
		if prop == "name" || prop == "length" {
			return true
		}
		if target.fn.props != nil {
			_, ok := target.fn.props[prop]
			return ok
		}
	}
	return false
}

func newHasOwnPropertyFn() jsValue {
	return nativeVal(func(r *jsRuntime, thisVal jsValue, args []jsValue) (jsValue, error) {
		if thisVal.kind == valUndefined || thisVal.kind == valNull {
			return r.throwValErr("TypeError", "Cannot convert undefined or null to object")
		}
		k := ""
		if len(args) > 0 {
			k = args[0].toString()
		}
		return boolVal(hasOwnPropValue(thisVal, k)), nil
	})
}

func sortedObjectKeys(o *jsObject) []string {
	if o == nil {
		return nil
	}
	src := o.props
	if o.scope != nil {
		src = o.scope.vars
	}
	if len(src) == 0 {
		return nil
	}
	type idxEntry struct {
		idx int
		key string
	}
	var indices []idxEntry
	var others []string
	for k := range src {
		if n, err := strconv.Atoi(k); err == nil && n >= 0 && strconv.Itoa(n) == k {
			indices = append(indices, idxEntry{idx: n, key: k})
		} else {
			others = append(others, k)
		}
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i].idx < indices[j].idx })
	sort.Strings(others)
	out := make([]string, 0, len(indices)+len(others))
	for _, it := range indices {
		out = append(out, it.key)
	}
	out = append(out, others...)
	return out
}

func (rt *jsRuntime) getProtoProp(proto *jsObject, prop string) (jsValue, bool) {
	if proto != nil && proto.props != nil {
		if v, ok := proto.props[prop]; ok {
			return v, true
		}
	}
	if rt != nil && rt.objProto != nil && proto != rt.objProto && rt.objProto.props != nil {
		if v, ok := rt.objProto.props[prop]; ok {
			return v, true
		}
	}
	return jsUndefinedVal, false
}

func (rt *jsRuntime) getProperty(target jsValue, prop string) jsValue {
	if prop == "__proto__" || prop == "constructor" {
		if target.kind == valObject && target.obj != nil {
			if target.obj.scope != nil {
				if v, ok := target.obj.scope.vars[prop]; ok {
					return v
				}
			} else if target.obj.props != nil {
				if v, ok := target.obj.props[prop]; ok {
					return v
				}
			}
		}
		return jsUndefinedVal
	}
	switch target.kind {
	case valObject:
		if target.obj != nil {
			if target.obj.scope != nil {
				if v, ok := target.obj.scope.vars[prop]; ok {
					return v
				}
			}
			if target.obj.props != nil {
				if v, ok := target.obj.props[prop]; ok {
					return v
				}
			}
			if prop == "toString" && target.obj.isError {
				return nativeVal(func(_ *jsRuntime, thisVal jsValue, _ []jsValue) (jsValue, error) {
					return jsValue{kind: valString, str: thisVal.toString()}, nil
				})
			}
			if prop == "hasOwnProperty" {
				return newHasOwnPropertyFn()
			}
			if v, ok := rt.getProtoProp(rt.objProto, prop); ok {
				return v
			}
			return jsUndefinedVal
		}
	case valNumber:
		if v := numberProperty(target.num, prop); v.kind != valUndefined {
			return v
		}
		if v, ok := rt.getProtoProp(rt.numProto, prop); ok {
			return v
		}
	case valFunc:
		if target.fn != nil {
			if v := funcProperty(target.fn, prop); v.kind != valUndefined {
				return v
			}
			if v, ok := rt.getProtoProp(rt.funcProto, prop); ok {
				return v
			}
		}
	case valString:
		if v := stringProperty(target.str, prop); v.kind != valUndefined {
			return v
		}
		if v, ok := rt.getProtoProp(rt.strProto, prop); ok {
			return v
		}
	case valArray:
		if target.arr != nil {
			if v := arrayProperty(target.arr, prop); v.kind != valUndefined {
				return v
			}
			if v, ok := rt.getProtoProp(rt.arrProto, prop); ok {
				return v
			}
		}
	case valRegExp:
		if target.re != nil {
			if v := regexpProperty(target.re, prop); v.kind != valUndefined {
				return v
			}
			if v, ok := rt.getProtoProp(rt.reProto, prop); ok {
				return v
			}
		}
	}
	return jsUndefinedVal
}

func nativeVal(fn jsNativeFn) jsValue {
	return jsValue{kind: valFunc, fn: &jsFunc{native: fn}}
}

func numberProperty(n float64, prop string) jsValue {
	switch prop {
	case "toFixed":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			digits := 0
			if len(args) > 0 && args[0].kind != valUndefined {
				df := args[0].toNumber()
				if !math.IsNaN(df) {
					if math.IsInf(df, 0) || df < 0 || df > 100 {
						return rt.throwValErr("RangeError", "toFixed() digits argument must be between 0 and 100")
					}
					digits = int(math.Trunc(df))
				}
			}
			if math.IsNaN(n) {
				return jsValue{kind: valString, str: "NaN"}, nil
			}
			if math.IsInf(n, 1) {
				return jsValue{kind: valString, str: "Infinity"}, nil
			}
			if math.IsInf(n, -1) {
				return jsValue{kind: valString, str: "-Infinity"}, nil
			}
			val := n
			if val == 0 {
				val = 0 // normalize -0 to +0
			}
			return jsValue{kind: valString, str: strconv.FormatFloat(val, 'f', digits, 64)}, nil
		})
	case "toString":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			radix := 10
			if len(args) > 0 && args[0].kind != valUndefined {
				rf := args[0].toNumber()
				if math.IsNaN(rf) || math.IsInf(rf, 0) {
					return rt.throwValErr("RangeError", "toString() radix argument must be between 2 and 36")
				}
				radix = int(math.Trunc(rf))
				if radix < 2 || radix > 36 {
					return rt.throwValErr("RangeError", "toString() radix argument must be between 2 and 36")
				}
			}
			if radix == 10 || math.IsNaN(n) || math.IsInf(n, 0) {
				return jsValue{kind: valString, str: (jsValue{kind: valNumber, num: n}).toString()}, nil
			}
			if n == 0 {
				return jsValue{kind: valString, str: "0"}, nil
			}
			neg := n < 0
			absVal := math.Abs(n)
			intPart := math.Trunc(absVal)
			fracPart := absVal - intPart
			var b strings.Builder
			if neg {
				b.WriteByte('-')
			}
			if intPart <= float64(math.MaxInt64) {
				b.WriteString(strconv.FormatInt(int64(intPart), radix))
			} else {
				// Large integer formatting in arbitrary base
				var rev []byte
				rem := intPart
				baseF := float64(radix)
				const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
				for rem >= 1 {
					q := math.Floor(rem / baseF)
					d := int(rem - q*baseF)
					if d < 0 {
						d = 0
					} else if d >= radix {
						d = radix - 1
					}
					rev = append(rev, digits[d])
					rem = q
				}
				for i := len(rev) - 1; i >= 0; i-- {
					b.WriteByte(rev[i])
				}
			}
			if fracPart > 0 {
				b.WriteByte('.')
				const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
				baseF := float64(radix)
				for step := 0; step < 52 && fracPart > 0; step++ {
					fracPart *= baseF
					d := int(math.Trunc(fracPart))
					if d >= radix {
						d = radix - 1
					}
					b.WriteByte(digits[d])
					fracPart -= float64(d)
				}
			}
			return jsValue{kind: valString, str: b.String()}, nil
		})
	case "valueOf":
		return nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
			return jsValue{kind: valNumber, num: n}, nil
		})
	}
	return jsUndefinedVal
}

func funcProperty(fn *jsFunc, prop string) jsValue {
	if fn.props != nil {
		if v, ok := fn.props[prop]; ok {
			return v
		}
	}
	switch prop {
	case "name":
		return jsValue{kind: valString, str: fn.name}
	case "length":
		cnt := 0
		for _, p := range fn.params {
			if p.rest || p.defaultVal != nil {
				break
			}
			cnt++
		}
		cnt -= len(fn.boundArgs)
		if cnt < 0 {
			cnt = 0
		}
		return jsValue{kind: valNumber, num: float64(cnt)}
	case "call":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			callThis := jsUndefinedVal
			var callArgs []jsValue
			if len(args) > 0 {
				callThis = args[0]
				callArgs = args[1:]
			}
			return rt.callFunc(fn, callThis, callArgs)
		})
	case "apply":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			callThis := jsUndefinedVal
			if len(args) > 0 {
				callThis = args[0]
			}
			var callArgs []jsValue
			if len(args) >= 2 && args[1].kind != valUndefined && args[1].kind != valNull {
				if args[1].kind == valArray && args[1].arr != nil {
					callArgs = make([]jsValue, len(args[1].arr.items))
					copy(callArgs, args[1].arr.items)
				} else if args[1].kind == valObject && args[1].obj != nil {
					lv := args[1].obj.get("length")
					if lv.kind != valUndefined {
						nf := lv.toNumber()
						if !math.IsNaN(nf) && nf > 0 {
							if nf > maxJSArrayLen {
								return rt.throwValErr("RangeError", "Maximum call arguments exceeded")
							}
							n := int(math.Trunc(nf))
							callArgs = make([]jsValue, n)
							for i := 0; i < n; i++ {
								callArgs[i] = args[1].obj.get(strconv.Itoa(i))
							}
						}
					}
				} else {
					return rt.throwValErr("TypeError", "CreateListFromArrayLike called on non-object")
				}
			}
			return rt.callFunc(fn, callThis, callArgs)
		})
	case "bind":
		return nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			bindThis := jsUndefinedVal
			var extra []jsValue
			if len(args) > 0 {
				bindThis = args[0]
				extra = args[1:]
			}
			effThis := bindThis
			if fn.hasBound {
				effThis = fn.boundThis
			}
			var effArgs []jsValue
			if len(fn.boundArgs)+len(extra) > 0 {
				effArgs = make([]jsValue, 0, len(fn.boundArgs)+len(extra))
				effArgs = append(effArgs, fn.boundArgs...)
				effArgs = append(effArgs, extra...)
			}
			return jsValue{
				kind: valFunc,
				fn: &jsFunc{
					name:      "bound " + fn.name,
					params:    fn.params,
					body:      fn.body,
					env:       fn.env,
					native:    fn.native,
					arrow:     fn.arrow,
					hasBound:  true,
					boundThis: effThis,
					boundArgs: effArgs,
				},
			}, nil
		})
	}
	return jsUndefinedVal
}

func stringProperty(s string, prop string) jsValue {
	if prop == "length" {
		return jsValue{kind: valNumber, num: float64(utf8.RuneCountInString(s))}
	}
	switch prop {
	case "toLowerCase", "toUpperCase", "trim", "trimStart", "trimLeft", "trimEnd", "trimRight":
		return nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
			var out string
			switch prop {
			case "toLowerCase":
				out = strings.ToLower(s)
			case "toUpperCase":
				out = strings.ToUpper(s)
			case "trim":
				out = strings.TrimSpace(s)
			case "trimStart", "trimLeft":
				out = strings.TrimLeftFunc(s, unicode.IsSpace)
			default:
				out = strings.TrimRightFunc(s, unicode.IsSpace)
			}
			return jsValue{kind: valString, str: out}, nil
		})
	case "padStart", "padEnd":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 || args[0].kind == valUndefined {
				return jsValue{kind: valString, str: s}, nil
			}
			sRunes := []rune(s)
			targetFloat := args[0].toNumber()
			if math.IsNaN(targetFloat) || targetFloat <= float64(len(sRunes)) {
				return jsValue{kind: valString, str: s}, nil
			}
			if math.IsInf(targetFloat, 1) || targetFloat > float64(maxJSStringBytes) {
				return rt.throwStrLenErr()
			}
			targetLen := int(math.Trunc(targetFloat))
			padStr := " "
			if len(args) >= 2 && args[1].kind != valUndefined {
				var pok bool
				padStr, pok = args[1].toStringChecked()
				if !pok {
					return rt.throwStrLenErr()
				}
			}
			padRunes := []rune(padStr)
			if len(padRunes) == 0 {
				return jsValue{kind: valString, str: s}, nil
			}
			need := targetLen - len(sRunes)
			fullReps := need / len(padRunes)
			rem := need % len(padRunes)
			projectedBytes := int64(len(s)) + int64(fullReps)*int64(len(padStr)) + int64(len(string(padRunes[:rem])))
			if projectedBytes > maxJSStringBytes {
				return rt.throwStrLenErr()
			}
			var b strings.Builder
			b.Grow(int(projectedBytes))
			if prop == "padEnd" {
				b.WriteString(s)
			}
			if fullReps > 0 {
				b.WriteString(strings.Repeat(padStr, fullReps))
			}
			if rem > 0 {
				b.WriteString(string(padRunes[:rem]))
			}
			if prop == "padStart" {
				b.WriteString(s)
			}
			return jsValue{kind: valString, str: b.String()}, nil
		})
	case "indexOf":
		return nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 {
				return jsValue{kind: valNumber, num: -1}, nil
			}
			sub := args[0].toString()
			if len(args) >= 2 && args[1].kind != valUndefined {
				sRunes := []rune(s)
				pos := min(max(0, int(math.Trunc(args[1].toNumber()))), len(sRunes))
				tail := string(sRunes[pos:])
				byteIdx := strings.Index(tail, sub)
				if byteIdx < 0 {
					return jsValue{kind: valNumber, num: -1}, nil
				}
				return jsValue{kind: valNumber, num: float64(pos + utf8.RuneCountInString(tail[:byteIdx]))}, nil
			}
			byteIdx := strings.Index(s, sub)
			if byteIdx < 0 {
				return jsValue{kind: valNumber, num: -1}, nil
			}
			return jsValue{kind: valNumber, num: float64(utf8.RuneCountInString(s[:byteIdx]))}, nil
		})
	case "lastIndexOf":
		return nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 {
				return jsValue{kind: valNumber, num: -1}, nil
			}
			sub := args[0].toString()
			sRunes := []rune(s)
			subRunes := []rune(sub)
			maxPos := len(sRunes)
			if len(args) >= 2 && args[1].kind != valUndefined {
				pf := args[1].toNumber()
				if !math.IsNaN(pf) {
					maxPos = min(maxPos, max(0, int(math.Trunc(pf))))
				}
			}
			for i := min(maxPos, len(sRunes)-len(subRunes)); i >= 0; i-- {
				matched := true
				for j := 0; j < len(subRunes); j++ {
					if sRunes[i+j] != subRunes[j] {
						matched = false
						break
					}
				}
				if matched {
					return jsValue{kind: valNumber, num: float64(i)}, nil
				}
			}
			return jsValue{kind: valNumber, num: -1}, nil
		})
	case "includes", "startsWith", "endsWith":
		return nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 {
				return jsFalseVal, nil
			}
			sub := args[0].toString()
			switch prop {
			case "includes":
				return boolVal(strings.Contains(s, sub)), nil
			case "startsWith":
				return boolVal(strings.HasPrefix(s, sub)), nil
			default:
				return boolVal(strings.HasSuffix(s, sub)), nil
			}
		})
	case "slice", "substring":
		return nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			runes := []rune(s)
			l := len(runes)
			start, end := 0, l
			if len(args) >= 1 && args[0].kind != valUndefined {
				start = int(args[0].toNumber())
				if start < 0 {
					if prop == "slice" {
						start = max(0, l+start)
					} else {
						start = 0
					}
				} else if start > l {
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
				} else if end > l {
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
	case "charAt", "charCodeAt":
		return nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			idx := 0
			if len(args) > 0 {
				idx = int(args[0].toNumber())
			}
			runes := []rune(s)
			if idx < 0 || idx >= len(runes) {
				if prop == "charAt" {
					return jsValue{kind: valString, str: ""}, nil
				}
				return jsValue{kind: valNumber, num: math.NaN()}, nil
			}
			if prop == "charAt" {
				return jsValue{kind: valString, str: string(runes[idx])}, nil
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
				return rt.throwValErr("RangeError", "Invalid count value")
			}
			count := int(n)
			if count == 0 || len(s) == 0 {
				return jsValue{kind: valString, str: ""}, nil
			}
			if count > maxJSStringBytes || int64(len(s))*int64(count) > maxJSStringBytes {
				return rt.throwStrLenErr()
			}
			return jsValue{kind: valString, str: strings.Repeat(s, count)}, nil
		})
	case "split":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			hasLimit := false
			limit := maxJSArrayLen + 1
			if len(args) >= 2 && args[1].kind != valUndefined {
				hasLimit = true
				lf := args[1].toNumber()
				if math.IsNaN(lf) || lf == 0 {
					return jsValue{kind: valArray, arr: &jsArray{items: []jsValue{}}}, nil
				}
				u := uint32(int64(math.Trunc(lf)))
				if u == 0 {
					return jsValue{kind: valArray, arr: &jsArray{items: []jsValue{}}}, nil
				}
				if int(u) <= maxJSArrayLen {
					limit = int(u)
				}
			}
			var parts []string
			if len(args) == 0 || args[0].kind == valUndefined {
				parts = []string{s}
			} else if args[0].kind == valRegExp && args[0].re != nil {
				fetchMax := maxJSArrayLen + 2
				if hasLimit && limit <= maxJSArrayLen {
					fetchMax = limit + 1
				}
				parts = args[0].re.re.Split(s, fetchMax)
				if hasLimit && len(parts) > limit {
					parts = parts[:limit]
				}
			} else {
				fetchMax := maxJSArrayLen + 2
				if hasLimit && limit <= maxJSArrayLen {
					fetchMax = limit + 1
				}
				parts = strings.SplitN(s, args[0].toString(), fetchMax)
				if hasLimit && len(parts) > limit {
					parts = parts[:limit]
				}
			}
			if len(parts) > maxJSArrayLen {
				return rt.throwValErr("RangeError", "Maximum split match count exceeded")
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
			if prop == "replaceAll" && args[0].kind == valRegExp && args[0].re != nil && !args[0].re.global {
				return rt.throwValErr("TypeError", "String.prototype.replaceAll called with a non-global RegExp argument")
			}
			if args[1].kind == valFunc && args[1].fn != nil {
				cb := args[1].fn
				if args[0].kind == valRegExp && args[0].re != nil {
					rx := args[0].re
					out, err := replaceRegexpCallbackBounded(rt, s, rx, cb, rx.global || prop == "replaceAll")
					if err != nil {
						return jsUndefinedVal, err
					}
					return jsValue{kind: valString, str: out}, nil
				}
				pat, pok := args[0].toStringChecked()
				if !pok {
					return rt.throwStrLenErr()
				}
				out, err := replaceStringCallbackBounded(rt, s, pat, cb, prop == "replaceAll")
				if err != nil {
					return jsUndefinedVal, err
				}
				return jsValue{kind: valString, str: out}, nil
			}
			repl, rok := args[1].toStringChecked()
			if !rok {
				return rt.throwStrLenErr()
			}
			if args[0].kind == valRegExp && args[0].re != nil {
				rx := args[0].re
				out, ok, iErr := replaceRegexpBounded(rt, s, rx, repl, rx.global || prop == "replaceAll")
				if iErr != nil {
					return jsUndefinedVal, iErr
				}
				if !ok {
					return rt.throwStrLenErr()
				}
				return jsValue{kind: valString, str: out}, nil
			}
			pat, pok := args[0].toStringChecked()
			if !pok {
				return rt.throwStrLenErr()
			}
			if prop == "replaceAll" {
				if pat == "" {
					rc := utf8.RuneCountInString(s)
					if rc+1 > maxJSArrayLen {
						return rt.throwValErr("RangeError", "Maximum match count exceeded")
					}
					if int64(len(s))+(int64(rc)+1)*int64(len(repl)) > maxJSStringBytes {
						return rt.throwStrLenErr()
					}
				} else if cnt := strings.Count(s, pat); cnt > 0 {
					if cnt > maxJSArrayLen {
						return rt.throwValErr("RangeError", "Maximum match count exceeded")
					}
					if int64(len(s))+int64(cnt)*(int64(len(repl))-int64(len(pat))) > maxJSStringBytes {
						return rt.throwStrLenErr()
					}
				}
				return jsValue{kind: valString, str: strings.ReplaceAll(s, pat, repl)}, nil
			}
			idx := strings.Index(s, pat)
			if idx < 0 {
				return jsValue{kind: valString, str: s}, nil
			}
			if int64(len(s))-int64(len(pat))+int64(len(repl)) > maxJSStringBytes {
				return rt.throwStrLenErr()
			}
			return jsValue{kind: valString, str: s[:idx] + repl + s[idx+len(pat):]}, nil
		})
	case "match":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			var rx *jsRegExp
			if len(args) == 0 || args[0].kind == valUndefined {
				rv, err := rt.compileRegExp("", "")
				if err != nil {
					return jsUndefinedVal, err
				}
				rx = rv.re
			} else if args[0].kind == valRegExp && args[0].re != nil {
				rx = args[0].re
			} else {
				rv, err := rt.compileRegExp(args[0].toString(), "")
				if err != nil {
					return jsUndefinedVal, err
				}
				rx = rv.re
			}
			var sub []string
			if rx.global {
				sub = rx.re.FindAllString(s, maxJSArrayLen+1)
			} else {
				sub = rx.re.FindStringSubmatch(s)
			}
			if sub == nil {
				return jsNullVal, nil
			}
			if len(sub) > maxJSArrayLen {
				return rt.throwValErr("RangeError", "Maximum regex match count exceeded")
			}
			items := make([]jsValue, len(sub))
			for i, m := range sub {
				items[i] = jsValue{kind: valString, str: m}
			}
			return jsValue{kind: valArray, arr: &jsArray{items: items}}, nil
		})
	case "matchAll":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			var rx *jsRegExp
			if len(args) == 0 || args[0].kind == valUndefined {
				rv, err := rt.compileRegExp("", "g")
				if err != nil {
					return jsUndefinedVal, err
				}
				rx = rv.re
			} else if args[0].kind == valRegExp && args[0].re != nil {
				if !args[0].re.global {
					return rt.throwValErr("TypeError", "String.prototype.matchAll called with a non-global RegExp argument")
				}
				rx = args[0].re
			} else {
				rv, err := rt.compileRegExp(args[0].toString(), "g")
				if err != nil {
					return jsUndefinedVal, err
				}
				rx = rv.re
			}
			locs := rx.re.FindAllStringSubmatchIndex(s, maxJSArrayLen+1)
			if len(locs) > maxJSArrayLen {
				return rt.throwValErr("RangeError", "Maximum regex match count exceeded")
			}
			items := make([]jsValue, len(locs))
			for i, loc := range locs {
				if err := rt.checkInterrupt(); err != nil {
					return jsUndefinedVal, err
				}
				numGroups := len(loc) / 2
				mItems := make([]jsValue, numGroups)
				for g := 0; g < numGroups; g++ {
					if loc[2*g] >= 0 {
						mItems[g] = jsValue{kind: valString, str: s[loc[2*g]:loc[2*g+1]]}
					} else {
						mItems[g] = jsUndefinedVal
					}
				}
				items[i] = jsValue{kind: valArray, arr: &jsArray{items: mItems}}
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

func replaceRegexpCallbackBounded(rt *jsRuntime, s string, rx *jsRegExp, cb *jsFunc, all bool) (string, error) {
	limit := 1
	if all {
		limit = maxJSArrayLen + 1
	}
	matches := rx.re.FindAllStringSubmatchIndex(s, limit)
	if len(matches) == 0 {
		return s, nil
	}
	if len(matches) > maxJSArrayLen {
		return "", rt.throwErr("RangeError", "Maximum regex match count exceeded")
	}
	subNames := rx.re.SubexpNames()
	hasNamed := false
	for _, nm := range subNames[1:] {
		if nm != "" {
			hasNamed = true
			break
		}
	}

	var b strings.Builder
	lastEnd := 0
	prevBytePos := 0
	runeOffset := 0
	wholeVal := jsValue{kind: valString, str: s}

	for _, loc := range matches {
		if err := rt.checkInterrupt(); err != nil {
			return "", err
		}
		mStart, mEnd := loc[0], loc[1]
		if !appendBoundedString(&b, s[lastEnd:mStart]) {
			return "", rt.throwErr("RangeError", "Invalid string length")
		}
		runeOffset += utf8.RuneCountInString(s[prevBytePos:mStart])
		prevBytePos = mStart

		numGroups := len(loc) / 2
		cbArgs := make([]jsValue, 0, numGroups+3)
		cbArgs = append(cbArgs, jsValue{kind: valString, str: s[mStart:mEnd]})
		var groupsObj *jsObject
		if hasNamed {
			groupsObj = rt.NewObject()
		}
		for g := 1; g < numGroups; g++ {
			var gv jsValue
			if loc[2*g] >= 0 {
				gv = jsValue{kind: valString, str: s[loc[2*g]:loc[2*g+1]]}
			} else {
				gv = jsUndefinedVal
			}
			cbArgs = append(cbArgs, gv)
			if hasNamed && g < len(subNames) && subNames[g] != "" {
				groupsObj.setVal(subNames[g], gv)
			}
		}
		cbArgs = append(cbArgs, jsValue{kind: valNumber, num: float64(runeOffset)}, wholeVal)
		if hasNamed {
			cbArgs = append(cbArgs, jsValue{kind: valObject, obj: groupsObj})
		}

		repVal, err := rt.callFunc(cb, jsUndefinedVal, cbArgs)
		if err != nil {
			return "", err
		}
		repStr, ok := repVal.toStringChecked()
		if !ok || !appendBoundedString(&b, repStr) {
			return "", rt.throwErr("RangeError", "Invalid string length")
		}
		lastEnd = mEnd
	}
	if !appendBoundedString(&b, s[lastEnd:]) {
		return "", rt.throwErr("RangeError", "Invalid string length")
	}
	return b.String(), nil
}

func replaceStringCallbackBounded(rt *jsRuntime, s string, pat string, cb *jsFunc, all bool) (string, error) {
	wholeVal := jsValue{kind: valString, str: s}
	patVal := jsValue{kind: valString, str: pat}
	if !all {
		idx := strings.Index(s, pat)
		if idx < 0 {
			return s, nil
		}
		offset := utf8.RuneCountInString(s[:idx])
		repVal, err := rt.callFunc(cb, jsUndefinedVal, []jsValue{patVal, {kind: valNumber, num: float64(offset)}, wholeVal})
		if err != nil {
			return "", err
		}
		repStr, ok := repVal.toStringChecked()
		if !ok || int64(len(s))-int64(len(pat))+int64(len(repStr)) > maxJSStringBytes {
			return "", rt.throwErr("RangeError", "Invalid string length")
		}
		return s[:idx] + repStr + s[idx+len(pat):], nil
	}

	var b strings.Builder
	if pat == "" {
		runes := []rune(s)
		if len(runes)+1 > maxJSArrayLen {
			return "", rt.throwErr("RangeError", "Maximum match count exceeded")
		}
		for i := 0; i <= len(runes); i++ {
			if err := rt.checkInterrupt(); err != nil {
				return "", err
			}
			repVal, err := rt.callFunc(cb, jsUndefinedVal, []jsValue{patVal, {kind: valNumber, num: float64(i)}, wholeVal})
			if err != nil {
				return "", err
			}
			repStr, ok := repVal.toStringChecked()
			if !ok || !appendBoundedString(&b, repStr) {
				return "", rt.throwErr("RangeError", "Invalid string length")
			}
			if i < len(runes) {
				if !appendBoundedString(&b, string(runes[i])) {
					return "", rt.throwErr("RangeError", "Invalid string length")
				}
			}
		}
		return b.String(), nil
	}

	searchPos := 0
	runeOffset := 0
	matchCount := 0
	patRuneLen := utf8.RuneCountInString(pat)
	for searchPos <= len(s) {
		if err := rt.checkInterrupt(); err != nil {
			return "", err
		}
		rel := strings.Index(s[searchPos:], pat)
		if rel < 0 {
			if !appendBoundedString(&b, s[searchPos:]) {
				return "", rt.throwErr("RangeError", "Invalid string length")
			}
			break
		}
		matchCount++
		if matchCount > maxJSArrayLen {
			return "", rt.throwErr("RangeError", "Maximum match count exceeded")
		}
		mStart := searchPos + rel
		if !appendBoundedString(&b, s[searchPos:mStart]) {
			return "", rt.throwErr("RangeError", "Invalid string length")
		}
		runeOffset += utf8.RuneCountInString(s[searchPos:mStart])
		repVal, err := rt.callFunc(cb, jsUndefinedVal, []jsValue{patVal, {kind: valNumber, num: float64(runeOffset)}, wholeVal})
		if err != nil {
			return "", err
		}
		repStr, ok := repVal.toStringChecked()
		if !ok || !appendBoundedString(&b, repStr) {
			return "", rt.throwErr("RangeError", "Invalid string length")
		}
		searchPos = mStart + len(pat)
		runeOffset += patRuneLen
	}
	return b.String(), nil
}

func flattenJSArray(rt *jsRuntime, items []jsValue, depth int, out *[]jsValue, seen map[*jsArray]bool) error {
	for _, it := range items {
		if err := rt.checkInterrupt(); err != nil {
			return err
		}
		if depth > 0 && it.kind == valArray && it.arr != nil {
			if seen[it.arr] {
				return rt.throwErr("RangeError", "Maximum array nesting exceeded")
			}
			seen[it.arr] = true
			err := flattenJSArray(rt, it.arr.items, depth-1, out, seen)
			delete(seen, it.arr)
			if err != nil {
				return err
			}
			continue
		}
		if len(*out)+1 > maxJSArrayLen {
			return rt.throwErr("RangeError", "Invalid array length")
		}
		*out = append(*out, it)
	}
	return nil
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
	arrVal := jsValue{kind: valArray, arr: arr}
	switch prop {
	case "push":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(arr.items)+len(args) > maxJSArrayLen {
				return rt.throwValErr("RangeError", "Invalid array length")
			}
			arr.items = append(arr.items, args...)
			return jsValue{kind: valNumber, num: float64(len(arr.items))}, nil
		})
	case "pop", "shift":
		return nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
			if len(arr.items) == 0 {
				return jsUndefinedVal, nil
			}
			if prop == "pop" {
				last := arr.items[len(arr.items)-1]
				arr.items[len(arr.items)-1] = jsUndefinedVal
				arr.items = arr.items[:len(arr.items)-1]
				return last, nil
			}
			first := arr.items[0]
			copy(arr.items, arr.items[1:])
			arr.items[len(arr.items)-1] = jsUndefinedVal
			arr.items = arr.items[:len(arr.items)-1]
			return first, nil
		})
	case "unshift":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 {
				return jsValue{kind: valNumber, num: float64(len(arr.items))}, nil
			}
			if len(arr.items)+len(args) > maxJSArrayLen {
				return rt.throwValErr("RangeError", "Invalid array length")
			}
			next := make([]jsValue, 0, len(args)+len(arr.items))
			next = append(next, args...)
			next = append(next, arr.items...)
			arr.items = next
			return jsValue{kind: valNumber, num: float64(len(arr.items))}, nil
		})
	case "concat":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			out := make([]jsValue, len(arr.items))
			copy(out, arr.items)
			for _, a := range args {
				if err := rt.checkInterrupt(); err != nil {
					return jsUndefinedVal, err
				}
				if a.kind == valArray && a.arr != nil {
					if len(out)+len(a.arr.items) > maxJSArrayLen {
						return rt.throwValErr("RangeError", "Invalid array length")
					}
					out = append(out, a.arr.items...)
				} else {
					if len(out)+1 > maxJSArrayLen {
						return rt.throwValErr("RangeError", "Invalid array length")
					}
					out = append(out, a)
				}
			}
			return jsValue{kind: valArray, arr: &jsArray{items: out}}, nil
		})
	case "reverse":
		return nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
			for i, j := 0, len(arr.items)-1; i < j; i, j = i+1, j-1 {
				arr.items[i], arr.items[j] = arr.items[j], arr.items[i]
			}
			return arrVal, nil
		})
	case "sort":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			var cmpFn *jsFunc
			if len(args) > 0 && args[0].kind != valUndefined {
				if args[0].kind != valFunc || args[0].fn == nil {
					return rt.throwValErr("TypeError", "compareFn must be a function")
				}
				cmpFn = args[0].fn
			}
			if len(arr.items) <= 1 {
				return arrVal, nil
			}
			defined := make([]jsValue, 0, len(arr.items))
			undefCount := 0
			for _, it := range arr.items {
				if it.kind == valUndefined {
					undefCount++
				} else {
					defined = append(defined, it)
				}
			}
			var sortErr error
			sort.SliceStable(defined, func(i, j int) bool {
				if cmpFn == nil {
					return defined[i].toString() < defined[j].toString()
				}
				if sortErr != nil {
					return false
				}
				res, err := rt.callFunc(cmpFn, jsUndefinedVal, []jsValue{defined[i], defined[j]})
				if err != nil {
					sortErr = err
					return false
				}
				return res.toNumber() < 0
			})
			if sortErr != nil {
				return jsUndefinedVal, sortErr
			}
			for i := 0; i < undefCount; i++ {
				defined = append(defined, jsUndefinedVal)
			}
			arr.items = defined
			return arrVal, nil
		})
	case "splice":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 {
				return jsValue{kind: valArray, arr: &jsArray{}}, nil
			}
			l := len(arr.items)
			start := int(math.Trunc(args[0].toNumber()))
			if start < 0 {
				start = max(0, l+start)
			} else if start > l {
				start = l
			}
			delCount := l - start
			if len(args) >= 2 {
				df := args[1].toNumber()
				if math.IsNaN(df) || df <= 0 {
					delCount = 0
				} else if !math.IsInf(df, 1) && int(math.Trunc(df)) <= l-start {
					delCount = int(math.Trunc(df))
				}
			}
			var insertItems []jsValue
			if len(args) > 2 {
				insertItems = args[2:]
			}
			if l-delCount+len(insertItems) > maxJSArrayLen {
				return rt.throwValErr("RangeError", "Invalid array length")
			}
			removed := make([]jsValue, delCount)
			copy(removed, arr.items[start:start+delCount])

			next := make([]jsValue, 0, l-delCount+len(insertItems))
			next = append(next, arr.items[:start]...)
			next = append(next, insertItems...)
			next = append(next, arr.items[start+delCount:]...)
			arr.items = next
			return jsValue{kind: valArray, arr: &jsArray{items: removed}}, nil
		})
	case "flat":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			depth := 1
			if len(args) > 0 && args[0].kind != valUndefined {
				df := args[0].toNumber()
				if math.IsNaN(df) || df <= 0 {
					depth = 0
				} else if math.IsInf(df, 1) || df > maxJSExportDepth {
					depth = maxJSExportDepth
				} else {
					depth = int(math.Trunc(df))
				}
			}
			out := make([]jsValue, 0, len(arr.items))
			if err := flattenJSArray(rt, arr.items, depth, &out, map[*jsArray]bool{arr: true}); err != nil {
				return jsUndefinedVal, err
			}
			return jsValue{kind: valArray, arr: &jsArray{items: out}}, nil
		})
	case "toString":
		return nativeVal(func(rt *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
			s, ok := arrVal.toStringChecked()
			if !ok {
				return rt.throwStrLenErr()
			}
			return jsValue{kind: valString, str: s}, nil
		})
	case "join":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			sep := ","
			if len(args) > 0 && args[0].kind != valUndefined {
				var sok bool
				sep, sok = args[0].toStringChecked()
				if !sok {
					return rt.throwStrLenErr()
				}
			}
			if len(arr.items) == 0 {
				return jsValue{kind: valString, str: ""}, nil
			}
			if len(arr.items) > 1 && len(sep) > 0 && int64(len(arr.items)-1)*int64(len(sep)) > maxJSStringBytes {
				return rt.throwStrLenErr()
			}
			var b strings.Builder
			seenArr := map[*jsArray]bool{arr: true}
			nodes := 0
			for i, it := range arr.items {
				if err := rt.checkInterrupt(); err != nil {
					return jsUndefinedVal, err
				}
				if i > 0 && len(sep) > 0 && !appendBoundedString(&b, sep) {
					return rt.throwStrLenErr()
				}
				if it.kind != valUndefined && it.kind != valNull && !it.writeToString(&b, seenArr, nil, 1, &nodes) {
					return rt.throwStrLenErr()
				}
			}
			return jsValue{kind: valString, str: b.String()}, nil
		})
	case "slice":
		return nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			l := len(arr.items)
			start, end := 0, l
			if len(args) >= 1 && args[0].kind != valUndefined {
				start = int(args[0].toNumber())
				if start < 0 {
					start = max(0, l+start)
				} else if start > l {
					start = l
				}
			}
			if len(args) >= 2 && args[1].kind != valUndefined {
				end = int(args[1].toNumber())
				if end < 0 {
					end = max(0, l+end)
				} else if end > l {
					end = l
				}
			}
			end = max(start, end)
			cp := make([]jsValue, end-start)
			copy(cp, arr.items[start:end])
			return jsValue{kind: valArray, arr: &jsArray{items: cp}}, nil
		})
	case "indexOf", "includes":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			asBool := prop == "includes"
			if len(args) == 0 {
				if asBool {
					return jsFalseVal, nil
				}
				return jsValue{kind: valNumber, num: -1}, nil
			}
			start := 0
			if len(args) >= 2 && args[1].kind != valUndefined {
				start = int(math.Trunc(args[1].toNumber()))
				if start < 0 {
					start = max(0, len(arr.items)+start)
				}
			}
			for i := start; i < len(arr.items); i++ {
				if err := rt.checkInterrupt(); err != nil {
					return jsUndefinedVal, err
				}
				it := arr.items[i]
				if jsStrictEqual(it, args[0]) || (asBool && it.kind == valNumber && args[0].kind == valNumber && math.IsNaN(it.num) && math.IsNaN(args[0].num)) {
					if asBool {
						return jsTrueVal, nil
					}
					return jsValue{kind: valNumber, num: float64(i)}, nil
				}
			}
			if asBool {
				return jsFalseVal, nil
			}
			return jsValue{kind: valNumber, num: -1}, nil
		})
	case "find", "findIndex", "some", "every", "map", "filter", "forEach", "flatMap":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 || args[0].kind != valFunc || args[0].fn == nil {
				switch prop {
				case "map", "filter":
					return jsValue{kind: valArray, arr: &jsArray{}}, nil
				case "forEach":
					return jsUndefinedVal, nil
				case "flatMap":
					return rt.throwValErr("TypeError", "callback must be a function")
				default:
					return rt.throwValErr("TypeError", "predicate must be a function")
				}
			}
			thisArg := jsUndefinedVal
			if len(args) >= 2 {
				thisArg = args[1]
			}
			var out []jsValue
			if prop == "map" {
				out = make([]jsValue, len(arr.items))
			}
			for i, it := range arr.items {
				if err := rt.checkInterrupt(); err != nil {
					return jsUndefinedVal, err
				}
				v, err := rt.callFunc(args[0].fn, thisArg, []jsValue{it, {kind: valNumber, num: float64(i)}, arrVal})
				if err != nil {
					return jsUndefinedVal, err
				}
				switch prop {
				case "find":
					if v.toBool() {
						return it, nil
					}
				case "findIndex":
					if v.toBool() {
						return jsValue{kind: valNumber, num: float64(i)}, nil
					}
				case "some":
					if v.toBool() {
						return jsTrueVal, nil
					}
				case "every":
					if !v.toBool() {
						return jsFalseVal, nil
					}
				case "map":
					out[i] = v
				case "filter":
					if v.toBool() {
						out = append(out, it)
					}
				case "flatMap":
					if v.kind == valArray && v.arr != nil {
						if len(out)+len(v.arr.items) > maxJSArrayLen {
							return rt.throwValErr("RangeError", "Invalid array length")
						}
						out = append(out, v.arr.items...)
					} else {
						if len(out)+1 > maxJSArrayLen {
							return rt.throwValErr("RangeError", "Invalid array length")
						}
						out = append(out, v)
					}
				}
			}
			switch prop {
			case "find", "forEach":
				return jsUndefinedVal, nil
			case "findIndex":
				return jsValue{kind: valNumber, num: -1}, nil
			case "some":
				return jsFalseVal, nil
			case "every":
				return jsTrueVal, nil
			default:
				return jsValue{kind: valArray, arr: &jsArray{items: out}}, nil
			}
		})
	case "reduce", "reduceRight":
		return nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 || args[0].kind != valFunc || args[0].fn == nil {
				return rt.throwValErr("TypeError", "reducer must be a function")
			}
			right := prop == "reduceRight"
			var acc jsValue
			startIdx, step := 0, 1
			if right {
				startIdx, step = len(arr.items)-1, -1
			}
			if len(args) >= 2 {
				acc = args[1]
			} else {
				if len(arr.items) == 0 {
					return rt.throwValErr("TypeError", "Reduce of empty array with no initial value")
				}
				acc = arr.items[startIdx]
				startIdx += step
			}
			for i := startIdx; i >= 0 && i < len(arr.items); i += step {
				if err := rt.checkInterrupt(); err != nil {
					return jsUndefinedVal, err
				}
				var err error
				acc, err = rt.callFunc(args[0].fn, jsUndefinedVal, []jsValue{acc, arr.items[i], {kind: valNumber, num: float64(i)}, arrVal})
				if err != nil {
					return jsUndefinedVal, err
				}
			}
			return acc, nil
		})
	}
	return jsUndefinedVal
}

func regexpProperty(rx *jsRegExp, prop string) jsValue {
	switch prop {
	case "source":
		return jsValue{kind: valString, str: rx.pattern}
	case "flags":
		return jsValue{kind: valString, str: rx.flags}
	case "global":
		return boolVal(rx.global)
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

func jsParseInt(args []jsValue) float64 {
	if len(args) == 0 {
		return math.NaN()
	}
	s := strings.TrimLeftFunc(args[0].toString(), unicode.IsSpace)
	sign := 1.0
	if len(s) > 0 {
		if s[0] == '-' {
			sign = -1.0
			s = s[1:]
		} else if s[0] == '+' {
			s = s[1:]
		}
	}
	radix := 0
	if len(args) >= 2 && args[1].kind != valUndefined {
		rf := args[1].toNumber()
		if !math.IsNaN(rf) && !math.IsInf(rf, 0) {
			radix = int(int32(math.Trunc(rf)))
		}
		if radix != 0 && (radix < 2 || radix > 36) {
			return math.NaN()
		}
	}
	if radix == 0 || radix == 16 {
		if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
			s = s[2:]
			radix = 16
		}
	}
	if radix == 0 {
		radix = 10
	}
	var acc float64
	count := 0
	baseF := float64(radix)
	for i := 0; i < len(s); i++ {
		c := s[i]
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'z':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'Z':
			d = int(c-'A') + 10
		default:
			d = radix
		}
		if d >= radix {
			break
		}
		acc = acc*baseF + float64(d)
		count++
	}
	if count == 0 {
		return math.NaN()
	}
	if acc == 0 {
		if sign < 0 {
			return math.Copysign(0, -1)
		}
		return 0
	}
	return sign * acc
}

func jsParseFloat(args []jsValue) float64 {
	if len(args) == 0 {
		return math.NaN()
	}
	s := strings.TrimLeftFunc(args[0].toString(), unicode.IsSpace)
	if s == "" {
		return math.NaN()
	}
	if strings.HasPrefix(s, "+Infinity") || strings.HasPrefix(s, "Infinity") {
		return math.Inf(1)
	}
	if strings.HasPrefix(s, "-Infinity") {
		return math.Inf(-1)
	}
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digitsBefore := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		digitsBefore++
	}
	digitsAfter := 0
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			digitsAfter++
		}
	}
	if digitsBefore == 0 && digitsAfter == 0 {
		return math.NaN()
	}
	validEnd := i
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		expPos := i + 1
		if expPos < len(s) && (s[expPos] == '+' || s[expPos] == '-') {
			expPos++
		}
		expDigits := 0
		for expPos < len(s) && s[expPos] >= '0' && s[expPos] <= '9' {
			expPos++
			expDigits++
		}
		if expDigits > 0 {
			validEnd = expPos
		}
	}
	f, err := strconv.ParseFloat(s[:validEnd], 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return f
		}
		return math.NaN()
	}
	return f
}

func isURIUnescapedByte(b byte) bool {
	if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') {
		return true
	}
	switch b {
	case '-', '_', '.', '!', '~', '*', '\'', '(', ')':
		return true
	}
	return false
}

func fromHexByte(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	default:
		return 0, false
	}
}

func encodeURIComponentRFC3986(s string) (string, bool, bool) {
	if !utf8.ValidString(s) {
		return "", false, true
	}
	const upperHex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if isURIUnescapedByte(ch) {
			if b.Len()+1 > maxJSStringBytes {
				return "", true, false
			}
			b.WriteByte(ch)
		} else {
			if b.Len()+3 > maxJSStringBytes {
				return "", true, false
			}
			b.WriteByte('%')
			b.WriteByte(upperHex[ch>>4])
			b.WriteByte(upperHex[ch&0x0F])
		}
	}
	return b.String(), false, false
}

func decodeURIComponentRFC3986(s string) (string, bool) {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '%' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if i+2 >= len(s) {
			return "", false
		}
		hi, ok1 := fromHexByte(s[i+1])
		lo, ok2 := fromHexByte(s[i+2])
		if !ok1 || !ok2 {
			return "", false
		}
		lead := (hi << 4) | lo
		if lead < 0x80 {
			b.WriteByte(lead)
			i += 3
			continue
		}
		var need int
		switch {
		case lead >= 0xC2 && lead <= 0xDF:
			need = 2
		case lead >= 0xE0 && lead <= 0xEF:
			need = 3
		case lead >= 0xF0 && lead <= 0xF4:
			need = 4
		default:
			return "", false
		}
		if i+3*need > len(s) {
			return "", false
		}
		var seq [4]byte
		seq[0] = lead
		for k := 1; k < need; k++ {
			off := i + 3*k
			if s[off] != '%' {
				return "", false
			}
			chi, hok1 := fromHexByte(s[off+1])
			clo, hok2 := fromHexByte(s[off+2])
			if !hok1 || !hok2 {
				return "", false
			}
			cb := (chi << 4) | clo
			if (cb & 0xC0) != 0x80 {
				return "", false
			}
			seq[k] = cb
		}
		rn, wid := utf8.DecodeRune(seq[:need])
		if rn == utf8.RuneError || wid != need {
			return "", false
		}
		b.Write(seq[:need])
		i += 3 * need
	}
	return b.String(), true
}

func (rt *jsRuntime) installBuiltins() {
	rt.objProto = rt.NewObject()
	rt.objProto.setVal("hasOwnProperty", newHasOwnPropertyFn())
	rt.arrProto = rt.NewObject()
	rt.strProto = rt.NewObject()
	rt.numProto = rt.NewObject()
	rt.reProto = rt.NewObject()
	rt.funcProto = rt.NewObject()
	rt.globalObj = &jsObject{scope: rt.global, props: make(map[string]jsValue)}
	rt.global.define("globalThis", jsValue{kind: valObject, obj: rt.globalObj}, false)

	rt.global.define("NaN", jsValue{kind: valNumber, num: math.NaN()}, true)
	rt.global.define("Infinity", jsValue{kind: valNumber, num: math.Inf(1)}, true)

	strCtor := &jsFunc{
		name: "String",
		native: func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 {
				return jsValue{kind: valString, str: ""}, nil
			}
			s, ok := args[0].toStringChecked()
			if !ok {
				return r.throwStrLenErr()
			}
			return jsValue{kind: valString, str: s}, nil
		},
		props: map[string]jsValue{
			"prototype": {kind: valObject, obj: rt.strProto},
			"fromCharCode": nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
				if len(args) == 0 {
					return jsValue{kind: valString, str: ""}, nil
				}
				if len(args) > maxJSStringBytes {
					return r.throwStrLenErr()
				}
				units := make([]uint16, len(args))
				for i, a := range args {
					n := a.toNumber()
					if !math.IsNaN(n) && !math.IsInf(n, 0) {
						units[i] = uint16(uint32(int64(math.Trunc(n))))
					}
				}
				out := string(utf16.Decode(units))
				if len(out) > maxJSStringBytes {
					return r.throwStrLenErr()
				}
				return jsValue{kind: valString, str: out}, nil
			}),
		},
	}
	rt.global.define("String", jsValue{kind: valFunc, fn: strCtor}, false)

	parseIntFn := nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		return jsValue{kind: valNumber, num: jsParseInt(args)}, nil
	})
	parseFloatFn := nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		return jsValue{kind: valNumber, num: jsParseFloat(args)}, nil
	})

	numCtor := &jsFunc{
		name: "Number",
		native: func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 {
				return jsValue{kind: valNumber, num: 0}, nil
			}
			return jsValue{kind: valNumber, num: args[0].toNumber()}, nil
		},
		props: map[string]jsValue{
			"prototype":         {kind: valObject, obj: rt.numProto},
			"NaN":               {kind: valNumber, num: math.NaN()},
			"POSITIVE_INFINITY": {kind: valNumber, num: math.Inf(1)},
			"NEGATIVE_INFINITY": {kind: valNumber, num: math.Inf(-1)},
			"MAX_SAFE_INTEGER":  {kind: valNumber, num: 9007199254740991},
			"MIN_SAFE_INTEGER":  {kind: valNumber, num: -9007199254740991},
			"EPSILON":           {kind: valNumber, num: 2.220446049250313e-16},
			"isNaN": nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
				return boolVal(len(args) > 0 && args[0].kind == valNumber && math.IsNaN(args[0].num)), nil
			}),
			"isFinite": nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
				return boolVal(len(args) > 0 && args[0].kind == valNumber && !math.IsNaN(args[0].num) && !math.IsInf(args[0].num, 0)), nil
			}),
			"isInteger": nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
				if len(args) == 0 || args[0].kind != valNumber {
					return jsFalseVal, nil
				}
				n := args[0].num
				return boolVal(!math.IsNaN(n) && !math.IsInf(n, 0) && math.Trunc(n) == n), nil
			}),
			"parseInt":   parseIntFn,
			"parseFloat": parseFloatFn,
		},
	}
	rt.global.define("Number", jsValue{kind: valFunc, fn: numCtor}, false)

	rt.global.define("Boolean", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		return boolVal(len(args) > 0 && args[0].toBool()), nil
	}), false)

	rt.global.define("parseInt", parseIntFn, false)
	rt.global.define("parseFloat", parseFloatFn, false)

	rt.global.define("isNaN", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		return boolVal(len(args) == 0 || math.IsNaN(args[0].toNumber())), nil
	}), false)

	rt.global.define("isFinite", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return jsFalseVal, nil
		}
		n := args[0].toNumber()
		return boolVal(!math.IsNaN(n) && !math.IsInf(n, 0)), nil
	}), false)

	uriArgStr := func(r *jsRuntime, args []jsValue) (string, error) {
		if len(args) == 0 {
			return "undefined", nil
		}
		raw, ok := args[0].toStringChecked()
		if !ok {
			_, err := r.throwStrLenErr()
			return "", err
		}
		return raw, nil
	}
	rt.global.define("encodeURIComponent", nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		raw, err := uriArgStr(r, args)
		if err != nil {
			return jsUndefinedVal, err
		}
		escaped, tooLong, badURI := encodeURIComponentRFC3986(raw)
		if badURI {
			return r.throwValErr("URIError", "URI malformed")
		}
		if tooLong {
			return r.throwStrLenErr()
		}
		return jsValue{kind: valString, str: escaped}, nil
	}), false)

	rt.global.define("decodeURIComponent", nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		raw, err := uriArgStr(r, args)
		if err != nil {
			return jsUndefinedVal, err
		}
		decoded, ok := decodeURIComponentRFC3986(raw)
		if !ok {
			return r.throwValErr("URIError", "URI malformed")
		}
		return jsValue{kind: valString, str: decoded}, nil
	}), false)

	for _, errType := range []string{"Error", "TypeError", "RangeError", "SyntaxError", "ReferenceError", "URIError"} {
		et := errType
		rt.global.define(et, nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			msg := ""
			if len(args) > 0 && args[0].kind != valUndefined {
				msg = args[0].toString()
			}
			return r.newErrorVal(et, msg), nil
		}), false)
	}

	reCtor := &jsFunc{
		name: "RegExp",
		native: func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			pat, flags := "", ""
			if len(args) >= 1 && args[0].kind != valUndefined {
				if args[0].kind == valRegExp && args[0].re != nil {
					pat = args[0].re.pattern
					flags = args[0].re.flags
				} else {
					pat = args[0].toString()
				}
			}
			if len(args) >= 2 && args[1].kind != valUndefined {
				flags = args[1].toString()
			}
			return r.compileRegExp(pat, flags)
		},
		props: map[string]jsValue{
			"prototype": {kind: valObject, obj: rt.reProto},
		},
	}
	rt.global.define("RegExp", jsValue{kind: valFunc, fn: reCtor}, false)

	// JSON
	jsonObj := rt.NewObject()
	jsonObj.setVal("stringify", nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 || args[0].kind == valUndefined || args[0].kind == valFunc {
			return jsUndefinedVal, nil
		}
		out, errKind, errMsg := stringifyJSValueBounded(r, args[0])
		if errKind != "" {
			if errKind == "interrupt" {
				return jsUndefinedVal, &jsInterruptedError{cause: r.loadInterruptCause()}
			}
			return r.throwValErr(errKind, errMsg)
		}
		return jsValue{kind: valString, str: out}, nil
	}))
	jsonObj.setVal("parse", nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return r.throwValErr("SyntaxError", "Unexpected end of JSON input")
		}
		raw, ok := args[0].toStringChecked()
		if !ok {
			return r.throwStrLenErr()
		}
		return parseJSONBounded(r, raw)
	}))
	rt.global.define("JSON", jsValue{kind: valObject, obj: jsonObj}, false)

	// Math
	mathObj := rt.NewObject()
	mathObj.setVal("PI", jsValue{kind: valNumber, num: math.Pi})
	mathObj.setVal("E", jsValue{kind: valNumber, num: math.E})
	unaryMathFn := func(fn func(float64) float64) jsValue {
		return nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 {
				return jsValue{kind: valNumber, num: math.NaN()}, nil
			}
			return jsValue{kind: valNumber, num: fn(args[0].toNumber())}, nil
		})
	}
	mathObj.setVal("floor", unaryMathFn(math.Floor))
	mathObj.setVal("ceil", unaryMathFn(math.Ceil))
	mathObj.setVal("round", unaryMathFn(math.Round))
	mathObj.setVal("trunc", unaryMathFn(math.Trunc))
	mathObj.setVal("abs", unaryMathFn(math.Abs))
	mathObj.setVal("sqrt", unaryMathFn(math.Sqrt))
	mathObj.setVal("pow", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) < 2 {
			return jsValue{kind: valNumber, num: math.NaN()}, nil
		}
		return jsValue{kind: valNumber, num: math.Pow(args[0].toNumber(), args[1].toNumber())}, nil
	}))
	mathObj.setVal("random", nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
		return jsValue{kind: valNumber, num: rand.Float64()}, nil
	}))
	minMaxMathFn := func(isMax bool) jsValue {
		return nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 {
				if isMax {
					return jsValue{kind: valNumber, num: math.Inf(-1)}, nil
				}
				return jsValue{kind: valNumber, num: math.Inf(1)}, nil
			}
			m := args[0].toNumber()
			if math.IsNaN(m) {
				return jsValue{kind: valNumber, num: math.NaN()}, nil
			}
			for _, a := range args[1:] {
				v := a.toNumber()
				if math.IsNaN(v) {
					return jsValue{kind: valNumber, num: math.NaN()}, nil
				}
				if (isMax && v > m) || (!isMax && v < m) {
					m = v
				}
			}
			return jsValue{kind: valNumber, num: m}, nil
		})
	}
	mathObj.setVal("max", minMaxMathFn(true))
	mathObj.setVal("min", minMaxMathFn(false))
	rt.global.define("Math", jsValue{kind: valObject, obj: mathObj}, false)

	// Constructible/callable Array & Array static helpers
	arrLenErr := func(r *jsRuntime) (jsValue, error) {
		return r.throwValErr("RangeError", "Invalid array length")
	}
	nullObjErr := func(r *jsRuntime) (jsValue, error) {
		return r.throwValErr("TypeError", "Cannot convert undefined or null to object")
	}
	arrCtor := &jsFunc{
		name: "Array",
		native: func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 1 && args[0].kind == valNumber {
				nf := args[0].num
				if math.IsNaN(nf) || math.IsInf(nf, 0) || nf < 0 || math.Trunc(nf) != nf || nf > maxJSArrayLen {
					return arrLenErr(r)
				}
				items := make([]jsValue, int(nf))
				for i := range items {
					items[i] = jsUndefinedVal
				}
				return jsValue{kind: valArray, arr: &jsArray{items: items}}, nil
			}
			if len(args) > maxJSArrayLen {
				return arrLenErr(r)
			}
			items := make([]jsValue, len(args))
			copy(items, args)
			return jsValue{kind: valArray, arr: &jsArray{items: items}}, nil
		},
		props: map[string]jsValue{
			"prototype": {kind: valObject, obj: rt.arrProto},
			"isArray": nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
				return boolVal(len(args) > 0 && args[0].kind == valArray), nil
			}),
			"of": nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
				if len(args) > maxJSArrayLen {
					return arrLenErr(r)
				}
				items := make([]jsValue, len(args))
				copy(items, args)
				return jsValue{kind: valArray, arr: &jsArray{items: items}}, nil
			}),
			"from": nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
				if len(args) == 0 || args[0].kind == valUndefined || args[0].kind == valNull {
					return nullObjErr(r)
				}
				var mapFn *jsFunc
				if len(args) >= 2 && args[1].kind != valUndefined {
					if args[1].kind != valFunc || args[1].fn == nil {
						return r.throwValErr("TypeError", "mapFn is not a function")
					}
					mapFn = args[1].fn
				}
				thisArg := jsUndefinedVal
				if len(args) >= 3 {
					thisArg = args[2]
				}
				src := args[0]
				var rawItems []jsValue
				switch src.kind {
				case valArray:
					if src.arr != nil {
						if len(src.arr.items) > maxJSArrayLen {
							return arrLenErr(r)
						}
						rawItems = make([]jsValue, len(src.arr.items))
						copy(rawItems, src.arr.items)
					}
				case valString:
					n := utf8.RuneCountInString(src.str)
					if n > maxJSArrayLen {
						return arrLenErr(r)
					}
					rawItems = make([]jsValue, 0, n)
					for _, rn := range src.str {
						rawItems = append(rawItems, jsValue{kind: valString, str: string(rn)})
					}
				case valObject:
					if src.obj != nil {
						lv := src.obj.get("length")
						if lv.kind != valUndefined {
							nf := lv.toNumber()
							if !math.IsNaN(nf) && nf > 0 {
								if math.IsInf(nf, 1) || nf > maxJSArrayLen {
									return arrLenErr(r)
								}
								n := int(math.Trunc(nf))
								rawItems = make([]jsValue, n)
								for i := 0; i < n; i++ {
									rawItems[i] = src.obj.get(strconv.Itoa(i))
								}
							}
						}
					}
				}
				if rawItems == nil {
					rawItems = []jsValue{}
				}
				if mapFn == nil {
					return jsValue{kind: valArray, arr: &jsArray{items: rawItems}}, nil
				}
				out := make([]jsValue, len(rawItems))
				for i, it := range rawItems {
					if err := r.checkInterrupt(); err != nil {
						return jsUndefinedVal, err
					}
					mv, err := r.callFunc(mapFn, thisArg, []jsValue{it, {kind: valNumber, num: float64(i)}})
					if err != nil {
						return jsUndefinedVal, err
					}
					out[i] = mv
				}
				return jsValue{kind: valArray, arr: &jsArray{items: out}}, nil
			}),
		},
	}
	rt.global.define("Array", jsValue{kind: valFunc, fn: arrCtor}, false)

	// Object & Object.prototype
	objectIterFn := func(mode uint8) jsValue {
		// mode: 0=keys, 1=values, 2=entries
		return nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 || args[0].kind == valUndefined || args[0].kind == valNull {
				return nullObjErr(r)
			}
			var out []jsValue
			makePair := func(k string, v jsValue) jsValue {
				return jsValue{kind: valArray, arr: &jsArray{items: []jsValue{{kind: valString, str: k}, v}}}
			}
			switch args[0].kind {
			case valObject:
				if args[0].obj != nil {
					ks := sortedObjectKeys(args[0].obj)
					if len(ks) > maxJSArrayLen {
						return arrLenErr(r)
					}
					out = make([]jsValue, len(ks))
					for i, k := range ks {
						switch mode {
						case 0:
							out[i] = jsValue{kind: valString, str: k}
						case 1:
							out[i] = args[0].obj.get(k)
						default:
							out[i] = makePair(k, args[0].obj.get(k))
						}
					}
				}
			case valArray:
				if args[0].arr != nil {
					items := args[0].arr.items
					out = make([]jsValue, len(items))
					if mode == 1 {
						copy(out, items)
					} else {
						for i, it := range items {
							ks := strconv.Itoa(i)
							if mode == 0 {
								out[i] = jsValue{kind: valString, str: ks}
							} else {
								out[i] = makePair(ks, it)
							}
						}
					}
				}
			case valString:
				if mode == 0 {
					n := utf8.RuneCountInString(args[0].str)
					if n > maxJSArrayLen {
						return arrLenErr(r)
					}
					out = make([]jsValue, n)
					for i := 0; i < n; i++ {
						out[i] = jsValue{kind: valString, str: strconv.Itoa(i)}
					}
				} else {
					items, err := r.spreadIterValues(args[0])
					if err != nil {
						return jsUndefinedVal, err
					}
					if mode == 1 {
						out = items
					} else {
						out = make([]jsValue, len(items))
						for i, it := range items {
							out[i] = makePair(strconv.Itoa(i), it)
						}
					}
				}
			}
			if out == nil {
				out = []jsValue{}
			}
			return jsValue{kind: valArray, arr: &jsArray{items: out}}, nil
		})
	}

	objCtor := &jsFunc{
		name: "Object",
		native: func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) == 0 || args[0].kind == valUndefined || args[0].kind == valNull {
				return jsValue{kind: valObject, obj: r.NewObject()}, nil
			}
			switch args[0].kind {
			case valObject, valArray, valFunc, valRegExp:
				return args[0], nil
			default:
				return jsValue{kind: valObject, obj: r.NewObject()}, nil
			}
		},
		props: map[string]jsValue{
			"prototype": {kind: valObject, obj: rt.objProto},
			"keys":      objectIterFn(0),
			"values":    objectIterFn(1),
			"entries":   objectIterFn(2),
			"freeze": nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
				if len(args) == 0 {
					return jsUndefinedVal, nil
				}
				if args[0].kind == valObject && args[0].obj != nil && args[0].obj.scope == nil {
					args[0].obj.frozen = true
				} else if args[0].kind == valFunc && args[0].fn != nil {
					args[0].fn.frozenProps = true
				}
				return args[0], nil
			}),
			"isFrozen": nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
				if len(args) == 0 {
					return jsTrueVal, nil
				}
				switch args[0].kind {
				case valObject:
					if args[0].obj == nil {
						return jsTrueVal, nil
					}
					return boolVal(args[0].obj.frozen), nil
				case valFunc:
					if args[0].fn == nil {
						return jsTrueVal, nil
					}
					return boolVal(args[0].fn.frozenProps), nil
				case valArray, valRegExp:
					return jsFalseVal, nil
				default:
					return jsTrueVal, nil
				}
			}),
			"fromEntries": nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
				if len(args) == 0 || args[0].kind == valUndefined || args[0].kind == valNull {
					return nullObjErr(r)
				}
				items, err := r.spreadIterValues(args[0])
				if err != nil {
					return jsUndefinedVal, err
				}
				out := r.NewObject()
				for _, entry := range items {
					if err := r.checkInterrupt(); err != nil {
						return jsUndefinedVal, err
					}
					switch entry.kind {
					case valArray, valObject:
						out.setVal(r.getProperty(entry, "0").toString(), r.getProperty(entry, "1"))
					default:
						return r.throwValErr("TypeError", "Iterator value is not an entry object")
					}
				}
				return jsValue{kind: valObject, obj: out}, nil
			}),
			"hasOwn": nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
				if len(args) == 0 || args[0].kind == valUndefined || args[0].kind == valNull {
					return nullObjErr(r)
				}
				k := ""
				if len(args) >= 2 {
					k = args[1].toString()
				}
				return boolVal(hasOwnPropValue(args[0], k)), nil
			}),
			"assign": nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
				if len(args) == 0 || args[0].kind == valUndefined || args[0].kind == valNull {
					return nullObjErr(r)
				}
				dst := args[0]
				if dst.kind == valObject && dst.obj != nil {
					if dst.obj.frozen {
						return r.throwValErr("TypeError", "Cannot assign to read only property of frozen object")
					}
					for _, src := range args[1:] {
						if src.kind == valObject && src.obj != nil {
							for _, k := range sortedObjectKeys(src.obj) {
								dst.obj.setVal(k, src.obj.get(k))
							}
						}
					}
				}
				return dst, nil
			}),
		},
	}
	rt.global.define("Object", jsValue{kind: valFunc, fn: objCtor}, false)

	// Web-standard Base64 helpers (btoa / atob): Latin-1 U+0000..U+00FF <-> raw 0x00..0xFF
	rt.global.define("btoa", nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return r.throwValErr("TypeError", "btoa requires at least 1 argument")
		}
		s, ok := args[0].toStringChecked()
		if !ok {
			return r.throwStrLenErr()
		}
		buf := make([]byte, 0, len(s))
		for i := 0; i < len(s); {
			rn, wid := utf8.DecodeRuneInString(s[i:])
			if rn == utf8.RuneError && wid == 1 {
				buf = append(buf, s[i])
				i++
				continue
			}
			if rn < 0 || rn > 0xFF {
				return r.throwValErr("InvalidCharacterError", "btoa input contains characters outside Latin1 range")
			}
			buf = append(buf, byte(rn))
			i += wid
		}
		if base64.StdEncoding.EncodedLen(len(buf)) > maxJSStringBytes {
			return r.throwStrLenErr()
		}
		return jsValue{kind: valString, str: base64.StdEncoding.EncodeToString(buf)}, nil
	}), false)

	rt.global.define("atob", nativeVal(func(r *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) == 0 {
			return r.throwValErr("TypeError", "atob requires at least 1 argument")
		}
		s, ok := args[0].toStringChecked()
		if !ok {
			return r.throwStrLenErr()
		}
		var clean strings.Builder
		clean.Grow(len(s))
		for i := 0; i < len(s); i++ {
			switch s[i] {
			case ' ', '\t', '\n', '\r', '\f':
			default:
				clean.WriteByte(s[i])
			}
		}
		encoded := clean.String()
		if len(encoded)%4 == 0 {
			if strings.HasSuffix(encoded, "==") {
				encoded = encoded[:len(encoded)-2]
			} else if strings.HasSuffix(encoded, "=") {
				encoded = encoded[:len(encoded)-1]
			}
		}
		if len(encoded)%4 == 1 || strings.IndexByte(encoded, '=') >= 0 {
			return r.throwValErr("InvalidCharacterError", "atob input is not valid base64")
		}
		decoded, err := base64.RawStdEncoding.DecodeString(encoded)
		if err != nil {
			return r.throwValErr("InvalidCharacterError", "atob input is not valid base64")
		}
		utf8Len := 0
		for _, b := range decoded {
			if b < 0x80 {
				utf8Len++
			} else {
				utf8Len += 2
			}
		}
		if utf8Len > maxJSStringBytes {
			return r.throwStrLenErr()
		}
		var out strings.Builder
		out.Grow(utf8Len)
		for _, b := range decoded {
			out.WriteRune(rune(b))
		}
		return jsValue{kind: valString, str: out.String()}, nil
	}), false)

	// Constructible Date (`new Date()`, `new Date(ms)`, `new Date(isoStr)`, `Date()`, `Date.now()`, `Date.parse()`)
	dateCtor := &jsFunc{
		name: "Date",
		native: func(r *jsRuntime, thisVal jsValue, args []jsValue) (jsValue, error) {
			if thisVal.kind == valUndefined {
				return jsValue{kind: valString, str: time.Now().UTC().Format(time.RFC1123)}, nil
			}
			return newJSDateVal(r, parseJSDateArgs(args)), nil
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

	rt.builtinGlobals = make(map[string]jsValue, len(rt.global.vars)+4)
	for k, v := range rt.global.vars {
		rt.builtinGlobals[k] = v
	}
}

func parseJSONBounded(r *jsRuntime, raw string) (jsValue, error) {
	b := []byte(raw)
	if !json.Valid(b) {
		return r.throwValErr("SyntaxError", "Unexpected token in JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()

	nodes := 0
	var readVal func(depth int) (jsValue, error)
	readVal = func(depth int) (jsValue, error) {
		if r != nil {
			if err := r.checkInterrupt(); err != nil {
				return jsUndefinedVal, err
			}
		}
		nodes++
		if nodes > maxJSONParseNodes {
			return r.throwValErr("RangeError", "Maximum JSON node budget exceeded")
		}
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return r.throwValErr("SyntaxError", "Unexpected end of JSON input")
			}
			return r.throwValErr("SyntaxError", err.Error())
		}
		switch t := tok.(type) {
		case json.Delim:
			if depth >= maxJSONParseDepth {
				return r.throwValErr("RangeError", "Maximum JSON nesting depth exceeded")
			}
			switch t {
			case '{':
				obj := &jsObject{props: make(map[string]jsValue)}
				for dec.More() {
					if r != nil {
						if err := r.checkInterrupt(); err != nil {
							return jsUndefinedVal, err
						}
					}
					kt, kErr := dec.Token()
					if kErr != nil {
						return r.throwValErr("SyntaxError", kErr.Error())
					}
					keyStr, ok := kt.(string)
					if !ok {
						return r.throwValErr("SyntaxError", "Expected JSON object property name")
					}
					if len(keyStr) > maxJSStringBytes {
						return r.throwStrLenErr()
					}
					if _, exists := obj.props[keyStr]; !exists && len(obj.props) >= maxJSObjectKeys {
						return r.throwValErr("RangeError", "Maximum JSON object keys exceeded")
					}
					val, vErr := readVal(depth + 1)
					if vErr != nil {
						return jsUndefinedVal, vErr
					}
					obj.props[keyStr] = val
				}
				if _, cErr := dec.Token(); cErr != nil {
					return r.throwValErr("SyntaxError", cErr.Error())
				}
				return jsValue{kind: valObject, obj: obj}, nil
			case '[':
				var items []jsValue
				for dec.More() {
					if len(items) >= maxJSArrayLen {
						return r.throwValErr("RangeError", "Maximum JSON array length exceeded")
					}
					elem, eErr := readVal(depth + 1)
					if eErr != nil {
						return jsUndefinedVal, eErr
					}
					items = append(items, elem)
				}
				if _, cErr := dec.Token(); cErr != nil {
					return r.throwValErr("SyntaxError", cErr.Error())
				}
				if items == nil {
					items = []jsValue{}
				}
				return jsValue{kind: valArray, arr: &jsArray{items: items}}, nil
			default:
				return r.throwValErr("SyntaxError", "Unexpected JSON delimiter")
			}
		case nil:
			return jsNullVal, nil
		case bool:
			return boolVal(t), nil
		case json.Number:
			f, pErr := strconv.ParseFloat(string(t), 64)
			if pErr != nil && !errors.Is(pErr, strconv.ErrRange) {
				return r.throwValErr("SyntaxError", pErr.Error())
			}
			return jsValue{kind: valNumber, num: f}, nil
		case string:
			if len(t) > maxJSStringBytes {
				return r.throwStrLenErr()
			}
			return jsValue{kind: valString, str: t}, nil
		default:
			return r.throwValErr("SyntaxError", "Unexpected JSON token")
		}
	}

	val, err := readVal(0)
	if err != nil {
		return jsUndefinedVal, err
	}
	if _, extraErr := dec.Token(); !errors.Is(extraErr, io.EOF) {
		return r.throwValErr("SyntaxError", "Unexpected trailing token in JSON")
	}
	return val, nil
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
	return float64(time.Date(year, time.Month(month), day, hour, min, sec, msec*int(time.Millisecond), time.UTC).UnixMilli())
}

func parseJSDateString(raw string) float64 {
	s := strings.TrimSpace(raw)
	if s == "" {
		return math.NaN()
	}
	for _, layout := range []string{
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
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return float64(t.UnixMilli())
		}
	}
	return math.NaN()
}

func newJSDateVal(_ *jsRuntime, epochMs float64) jsValue {
	d := &jsObject{
		isDate:  true,
		epochMs: epochMs,
		props:   make(map[string]jsValue, 18),
	}
	validTime := func() (time.Time, bool) {
		if math.IsNaN(d.epochMs) || math.IsInf(d.epochMs, 0) {
			return time.Time{}, false
		}
		return time.UnixMilli(int64(d.epochMs)).UTC(), true
	}
	d.setVal("toISOString", nativeVal(func(r *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
		t, ok := validTime()
		if !ok {
			return r.throwValErr("RangeError", "Invalid time value")
		}
		return jsValue{kind: valString, str: t.Format("2006-01-02T15:04:05.000Z")}, nil
	}))
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
	datePartFn := func(part uint8) jsValue {
		return nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
			t, ok := validTime()
			if !ok {
				return jsValue{kind: valNumber, num: math.NaN()}, nil
			}
			var v int
			switch part {
			case 0:
				v = t.Year()
			case 1:
				v = int(t.Month()) - 1
			case 2:
				v = t.Day()
			case 3:
				v = t.Hour()
			case 4:
				v = t.Minute()
			default:
				v = t.Second()
			}
			return jsValue{kind: valNumber, num: float64(v)}, nil
		})
	}
	for i, names := range [][2]string{
		{"getFullYear", "getUTCFullYear"},
		{"getMonth", "getUTCMonth"},
		{"getDate", "getUTCDate"},
		{"getHours", "getUTCHours"},
		{"getMinutes", "getUTCMinutes"},
		{"getSeconds", "getUTCSeconds"},
	} {
		fn := datePartFn(uint8(i))
		d.setVal(names[0], fn)
		d.setVal(names[1], fn)
	}
	return jsValue{kind: valObject, obj: d}
}

// --- Go <-> JS Marshaling (`json` struct tag aware) ---

func newStandaloneErrorVal(name, msg string) jsValue {
	o := &jsObject{
		isError: true,
		props: map[string]jsValue{
			"name":    {kind: valString, str: name},
			"message": {kind: valString, str: msg},
		},
	}
	return jsValue{kind: valObject, obj: o}
}

func throwConversionRangeError(msg string) {
	panic(&jsThrownError{val: newStandaloneErrorVal("RangeError", msg)})
}

func (rt *jsRuntime) ToValue(v any) jsValue {
	nodes := 0
	return goToJSValueBounded(v, 0, &nodes)
}

func goToJSValue(v any) jsValue {
	nodes := 0
	return goToJSValueBounded(v, 0, &nodes)
}

func goToJSValueBounded(v any, depth int, nodes *int) jsValue {
	*nodes++
	if *nodes > maxJSONParseNodes {
		throwConversionRangeError("Maximum converted JS node budget exceeded")
	}
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
	case []any:
		if depth >= maxJSONParseDepth {
			throwConversionRangeError("Maximum converted JS nesting depth exceeded")
		}
		if len(x) > maxJSArrayLen {
			throwConversionRangeError("Maximum converted JS array length exceeded")
		}
		items := make([]jsValue, len(x))
		for i, elem := range x {
			items[i] = goToJSValueBounded(elem, depth+1, nodes)
		}
		return jsValue{kind: valArray, arr: &jsArray{items: items}}
	case []string:
		if depth >= maxJSONParseDepth {
			throwConversionRangeError("Maximum converted JS nesting depth exceeded")
		}
		if len(x) > maxJSArrayLen {
			throwConversionRangeError("Maximum converted JS array length exceeded")
		}
		*nodes += len(x)
		if *nodes > maxJSONParseNodes {
			throwConversionRangeError("Maximum converted JS node budget exceeded")
		}
		items := make([]jsValue, len(x))
		for i, s := range x {
			items[i] = jsValue{kind: valString, str: s}
		}
		return jsValue{kind: valArray, arr: &jsArray{items: items}}
	case map[string]any:
		if depth >= maxJSONParseDepth {
			throwConversionRangeError("Maximum converted JS nesting depth exceeded")
		}
		if len(x) > maxJSObjectKeys {
			throwConversionRangeError("Maximum converted JS object keys exceeded")
		}
		o := &jsObject{props: make(map[string]jsValue, len(x))}
		for k, val := range x {
			o.props[k] = goToJSValueBounded(val, depth+1, nodes)
		}
		return jsValue{kind: valObject, obj: o}
	case map[string]string:
		if depth >= maxJSONParseDepth {
			throwConversionRangeError("Maximum converted JS nesting depth exceeded")
		}
		if len(x) > maxJSObjectKeys {
			throwConversionRangeError("Maximum converted JS object keys exceeded")
		}
		*nodes += len(x)
		if *nodes > maxJSONParseNodes {
			throwConversionRangeError("Maximum converted JS node budget exceeded")
		}
		o := &jsObject{props: make(map[string]jsValue, len(x))}
		for k, val := range x {
			o.props[k] = jsValue{kind: valString, str: val}
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
		if depth >= maxJSONParseDepth {
			throwConversionRangeError("Maximum converted JS nesting depth exceeded")
		}
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
			if _, exists := o.props[key]; !exists && len(o.props) >= maxJSObjectKeys {
				throwConversionRangeError("Maximum converted JS object keys exceeded")
			}
			o.props[key] = goToJSValueBounded(rv.Field(i).Interface(), depth+1, nodes)
		}
		return jsValue{kind: valObject, obj: o}
	case reflect.Slice, reflect.Array:
		if depth >= maxJSONParseDepth {
			throwConversionRangeError("Maximum converted JS nesting depth exceeded")
		}
		n := rv.Len()
		if n > maxJSArrayLen {
			throwConversionRangeError("Maximum converted JS array length exceeded")
		}
		items := make([]jsValue, n)
		for i := 0; i < n; i++ {
			items[i] = goToJSValueBounded(rv.Index(i).Interface(), depth+1, nodes)
		}
		return jsValue{kind: valArray, arr: &jsArray{items: items}}
	case reflect.Map:
		if depth >= maxJSONParseDepth {
			throwConversionRangeError("Maximum converted JS nesting depth exceeded")
		}
		if rv.Len() > maxJSObjectKeys {
			throwConversionRangeError("Maximum converted JS object keys exceeded")
		}
		o := &jsObject{props: make(map[string]jsValue, rv.Len())}
		iter := rv.MapRange()
		for iter.Next() {
			k := fmt.Sprint(iter.Key().Interface())
			if _, exists := o.props[k]; !exists && len(o.props) >= maxJSObjectKeys {
				throwConversionRangeError("Maximum converted JS object keys exceeded")
			}
			o.props[k] = goToJSValueBounded(iter.Value().Interface(), depth+1, nodes)
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

func replaceRegexpBounded(rt *jsRuntime, s string, rx *jsRegExp, repl string, global bool) (string, bool, error) {
	hasDollar := strings.Contains(repl, "$")
	if !global {
		var loc []int
		if hasDollar {
			loc = rx.re.FindStringSubmatchIndex(s)
		} else {
			loc = rx.re.FindStringIndex(s)
		}
		if loc == nil {
			return s, true, nil
		}
		if !hasDollar {
			if int64(len(s))-int64(loc[1]-loc[0])+int64(len(repl)) > maxJSStringBytes {
				return "", false, nil
			}
			return s[:loc[0]] + repl + s[loc[1]:], true, nil
		}
		var b strings.Builder
		if !appendBoundedString(&b, s[:loc[0]]) {
			return "", false, nil
		}
		if !appendExpandedRegexpRepl(&b, rx.re, repl, s, loc) {
			return "", false, nil
		}
		if !appendBoundedString(&b, s[loc[1]:]) {
			return "", false, nil
		}
		return b.String(), true, nil
	}

	var b strings.Builder
	hasAnchorOrWordBoundary := strings.Contains(rx.pattern, "^") ||
		strings.Contains(rx.pattern, `\A`) ||
		strings.Contains(rx.pattern, `\b`) ||
		strings.Contains(rx.pattern, `\B`)

	if hasAnchorOrWordBoundary {
		var matches [][]int
		if hasDollar {
			matches = rx.re.FindAllStringSubmatchIndex(s, maxJSArrayLen+1)
		} else {
			matches = rx.re.FindAllStringIndex(s, maxJSArrayLen+1)
		}
		if len(matches) == 0 {
			return s, true, nil
		}
		if len(matches) > maxJSArrayLen {
			return "", false, rt.throwErr("RangeError", "Maximum regex match count exceeded")
		}
		lastEnd := 0
		for _, loc := range matches {
			if err := rt.checkInterrupt(); err != nil {
				return "", false, err
			}
			if !appendBoundedString(&b, s[lastEnd:loc[0]]) {
				return "", false, nil
			}
			if hasDollar {
				if !appendExpandedRegexpRepl(&b, rx.re, repl, s, loc) {
					return "", false, nil
				}
			} else {
				if !appendBoundedString(&b, repl) {
					return "", false, nil
				}
			}
			lastEnd = loc[1]
		}
		if !appendBoundedString(&b, s[lastEnd:]) {
			return "", false, nil
		}
		return b.String(), true, nil
	}

	lastAppend := 0
	searchPos := 0
	prevNonEmptyEnd := -1
	emptyMatchCount := 0
	for searchPos <= len(s) {
		if err := rt.checkInterrupt(); err != nil {
			return "", false, err
		}
		var loc []int
		if hasDollar {
			loc = rx.re.FindStringSubmatchIndex(s[searchPos:])
		} else {
			loc = rx.re.FindStringIndex(s[searchPos:])
		}
		if loc == nil {
			break
		}
		for i := range loc {
			if loc[i] >= 0 {
				loc[i] += searchPos
			}
		}
		mStart, mEnd := loc[0], loc[1]
		if mStart == mEnd && mEnd == prevNonEmptyEnd {
			if mEnd >= len(s) {
				break
			}
			_, wid := utf8.DecodeRuneInString(s[mEnd:])
			searchPos = mEnd + wid
			continue
		}
		if mStart == mEnd {
			emptyMatchCount++
			if emptyMatchCount > maxJSArrayLen {
				return "", false, rt.throwErr("RangeError", "Maximum regex match count exceeded")
			}
		}
		if !appendBoundedString(&b, s[lastAppend:mStart]) {
			return "", false, nil
		}
		if hasDollar {
			if !appendExpandedRegexpRepl(&b, rx.re, repl, s, loc) {
				return "", false, nil
			}
		} else {
			if !appendBoundedString(&b, repl) {
				return "", false, nil
			}
		}
		lastAppend = mEnd
		if mEnd > mStart {
			prevNonEmptyEnd = mEnd
			searchPos = mEnd
		} else {
			prevNonEmptyEnd = -1
			if mEnd >= len(s) {
				break
			}
			_, wid := utf8.DecodeRuneInString(s[mEnd:])
			searchPos = mEnd + wid
		}
	}
	if lastAppend == 0 && b.Len() == 0 {
		return s, true, nil
	}
	if !appendBoundedString(&b, s[lastAppend:]) {
		return "", false, nil
	}
	return b.String(), true, nil
}

func appendExpandedRegexpRepl(b *strings.Builder, re *regexp.Regexp, repl string, src string, match []int) bool {
	numGroups := len(match) / 2
	for i := 0; i < len(repl); {
		idx := strings.IndexByte(repl[i:], '$')
		if idx < 0 {
			return appendBoundedString(b, repl[i:])
		}
		if idx > 0 {
			if !appendBoundedString(b, repl[i:i+idx]) {
				return false
			}
		}
		i += idx + 1
		if i >= len(repl) {
			return appendBoundedString(b, "$")
		}
		ch := repl[i]
		switch {
		case ch == '$':
			if !appendBoundedString(b, "$") {
				return false
			}
			i++
		case ch == '&':
			if !appendBoundedString(b, src[match[0]:match[1]]) {
				return false
			}
			i++
		case ch == '`':
			if !appendBoundedString(b, src[:match[0]]) {
				return false
			}
			i++
		case ch == '\'':
			if !appendBoundedString(b, src[match[1]:]) {
				return false
			}
			i++
		case ch >= '0' && ch <= '9':
			d1 := int(ch - '0')
			if i+1 < len(repl) && repl[i+1] >= '0' && repl[i+1] <= '9' {
				d2 := d1*10 + int(repl[i+1]-'0')
				if d2 > 0 && d2 < numGroups {
					if match[2*d2] >= 0 {
						if !appendBoundedString(b, src[match[2*d2]:match[2*d2+1]]) {
							return false
						}
					}
					i += 2
					continue
				}
			}
			if d1 < numGroups {
				if match[2*d1] >= 0 {
					if !appendBoundedString(b, src[match[2*d1]:match[2*d1+1]]) {
						return false
					}
				}
				i++
			} else {
				if !appendBoundedString(b, "$") {
					return false
				}
			}
		case ch == '<' || ch == '{':
			closeCh := byte('>')
			if ch == '{' {
				closeCh = '}'
			}
			end := strings.IndexByte(repl[i+1:], closeCh)
			if end >= 0 {
				name := repl[i+1 : i+1+end]
				subIdx := re.SubexpIndex(name)
				if subIdx >= 0 && subIdx < numGroups {
					if match[2*subIdx] >= 0 {
						if !appendBoundedString(b, src[match[2*subIdx]:match[2*subIdx+1]]) {
							return false
						}
					}
					i += end + 2
					continue
				}
				if ch == '{' {
					// Go ExpandString-style unknown named group expands to empty
					i += end + 2
					continue
				}
			}
			if !appendBoundedString(b, "$") {
				return false
			}
		default:
			if !appendBoundedString(b, "$") {
				return false
			}
		}
	}
	return true
}

func stringifyJSValueBounded(rt *jsRuntime, root jsValue) (string, string, string) {
	var b strings.Builder
	seenObj := make(map[*jsObject]bool)
	seenArr := make(map[*jsArray]bool)
	nodes := 0

	var writeVal func(v jsValue, depth int) (string, string)
	writeVal = func(v jsValue, depth int) (string, string) {
		if rt != nil {
			if err := rt.checkInterrupt(); err != nil {
				return "interrupt", err.Error()
			}
		}
		nodes++
		if depth > maxJSExportDepth {
			return "RangeError", "Maximum call stack size exceeded"
		}
		if nodes > maxJSExportNodes {
			return "RangeError", "Maximum JSON node budget exceeded"
		}
		switch v.kind {
		case valUndefined, valNull, valFunc:
			if !appendBoundedString(&b, "null") {
				return "RangeError", "Invalid string length"
			}
			return "", ""
		case valBool:
			lit := "false"
			if v.b {
				lit = "true"
			}
			if !appendBoundedString(&b, lit) {
				return "RangeError", "Invalid string length"
			}
			return "", ""
		case valNumber:
			if math.IsNaN(v.num) || math.IsInf(v.num, 0) {
				if !appendBoundedString(&b, "null") {
					return "RangeError", "Invalid string length"
				}
				return "", ""
			}
			var numStr string
			if v.num == math.Trunc(v.num) && v.num >= -9007199254740991 && v.num <= 9007199254740991 {
				numStr = strconv.FormatInt(int64(v.num), 10)
			} else {
				numStr = strconv.FormatFloat(v.num, 'g', -1, 64)
			}
			if !appendBoundedString(&b, numStr) {
				return "RangeError", "Invalid string length"
			}
			return "", ""
		case valString:
			enc, err := json.Marshal(v.str)
			if err != nil {
				return "TypeError", err.Error()
			}
			if b.Len()+len(enc) > maxJSStringBytes {
				return "RangeError", "Invalid string length"
			}
			b.Write(enc)
			return "", ""
		case valArray:
			if v.arr == nil || len(v.arr.items) == 0 {
				if !appendBoundedString(&b, "[]") {
					return "RangeError", "Invalid string length"
				}
				return "", ""
			}
			if seenArr[v.arr] {
				return "TypeError", "Converting circular structure to JSON"
			}
			seenArr[v.arr] = true
			defer delete(seenArr, v.arr)

			if !appendBoundedString(&b, "[") {
				return "RangeError", "Invalid string length"
			}
			for i, it := range v.arr.items {
				if i > 0 {
					if !appendBoundedString(&b, ",") {
						return "RangeError", "Invalid string length"
					}
				}
				if ek, em := writeVal(it, depth+1); ek != "" {
					return ek, em
				}
			}
			if !appendBoundedString(&b, "]") {
				return "RangeError", "Invalid string length"
			}
			return "", ""
		case valObject:
			if v.obj == nil {
				if !appendBoundedString(&b, "{}") {
					return "RangeError", "Invalid string length"
				}
				return "", ""
			}
			if v.obj.isDate {
				if math.IsNaN(v.obj.epochMs) || math.IsInf(v.obj.epochMs, 0) {
					if !appendBoundedString(&b, "null") {
						return "RangeError", "Invalid string length"
					}
					return "", ""
				}
				iso := `"` + time.UnixMilli(int64(v.obj.epochMs)).UTC().Format("2006-01-02T15:04:05.000Z") + `"`
				if !appendBoundedString(&b, iso) {
					return "RangeError", "Invalid string length"
				}
				return "", ""
			}
			if seenObj[v.obj] {
				return "TypeError", "Converting circular structure to JSON"
			}
			seenObj[v.obj] = true
			defer delete(seenObj, v.obj)

			if !appendBoundedString(&b, "{") {
				return "RangeError", "Invalid string length"
			}
			if len(v.obj.props) > 0 {
				keys := make([]string, 0, len(v.obj.props))
				for k, pv := range v.obj.props {
					if pv.kind != valUndefined && pv.kind != valFunc {
						keys = append(keys, k)
					}
				}
				sort.Strings(keys)
				for i, k := range keys {
					if i > 0 {
						if !appendBoundedString(&b, ",") {
							return "RangeError", "Invalid string length"
						}
					}
					kEnc, err := json.Marshal(k)
					if err != nil {
						return "TypeError", err.Error()
					}
					if b.Len()+len(kEnc)+1 > maxJSStringBytes {
						return "RangeError", "Invalid string length"
					}
					b.Write(kEnc)
					b.WriteByte(':')
					if ek, em := writeVal(v.obj.props[k], depth+1); ek != "" {
						return ek, em
					}
				}
			}
			if !appendBoundedString(&b, "}") {
				return "RangeError", "Invalid string length"
			}
			return "", ""
		default:
			if !appendBoundedString(&b, "null") {
				return "RangeError", "Invalid string length"
			}
			return "", ""
		}
	}

	if ek, em := writeVal(root, 0); ek != "" {
		return "", ek, em
	}
	return b.String(), "", ""
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
	}
	nodes := 0
	return v.exportBounded(nil, nil, 0, &nodes)
}

func (v jsValue) exportBounded(seenObj map[*jsObject]bool, seenArr map[*jsArray]bool, depth int, nodes *int) any {
	*nodes++
	if depth > maxJSExportDepth || *nodes > maxJSExportNodes {
		return nil
	}
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
		if seenArr != nil && seenArr[v.arr] {
			return nil
		}
		if seenArr == nil {
			seenArr = make(map[*jsArray]bool, 4)
		}
		seenArr[v.arr] = true
		defer delete(seenArr, v.arr)

		out := make([]any, len(v.arr.items))
		for i, it := range v.arr.items {
			if *nodes > maxJSExportNodes {
				break
			}
			out[i] = it.exportBounded(seenObj, seenArr, depth+1, nodes)
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
		if seenObj != nil && seenObj[v.obj] {
			return nil
		}
		if seenObj == nil {
			seenObj = make(map[*jsObject]bool, 4)
		}
		seenObj[v.obj] = true
		defer delete(seenObj, v.obj)

		out := make(map[string]any, len(v.obj.props))
		for k, pv := range v.obj.props {
			if *nodes > maxJSExportNodes {
				break
			}
			if pv.kind != valUndefined && pv.kind != valFunc {
				out[k] = pv.exportBounded(seenObj, seenArr, depth+1, nodes)
			}
		}
		return out
	default:
		return nil
	}
}
