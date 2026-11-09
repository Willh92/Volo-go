package plugin

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/security"
)

const (
	defaultVMStackLimit     = 512
	defaultVMPoolCap        = 4
	defaultVMMaxConcurrency = 8
	maxPooledVMUses         = 1000
	maxPluginFetchBodyBytes = 1 << 20 // 1 MiB cap on outbound HTTP responses
	maxPluginRateLimitKeys  = 2048
)

// ErrExecutionTimeout is returned when a plugin hook, route, slot, or cron exceeds its time budget.
var ErrExecutionTimeout = errors.New("plugin: execution interrupted due to timeout")

type pooledVM struct {
	vm                *jsRuntime
	exports           *jsObject
	hookHandlers      map[string][]*jsFunc
	directiveHandlers map[string]*jsFunc
	federationPublish *jsFunc
	federationDeliver *jsFunc
	hooksFrozen       bool
	uses              int
	reqCtx            context.Context
}

type pluginRateBucket struct {
	windowID    int64
	expiresUnix int64
	count       int
}

type gojaPluginRuntime struct {
	manifest     Manifest
	pluginDir    string
	program      *jsProgram
	localI18n    map[string]map[string]string
	kv           *KVStore
	logger       *PluginLogger
	hostI18n     *i18n.Catalog
	getConfig    func() map[string]any
	unsealSecret func(string) string
	getSiteInfo  func() map[string]any
	httpClient   *http.Client

	vmCreatedCount atomic.Uint64

	rateMu      sync.Mutex
	rateBuckets map[string]pluginRateBucket

	pool chan *pooledVM
	sem  chan struct{}
}

type gojaRuntimeOptions struct {
	manifest              Manifest
	pluginDir             string
	kv                    *KVStore
	logger                *PluginLogger
	hostI18n              *i18n.Catalog
	getConfig             func() map[string]any
	unsealSecret          func(string) string
	getSiteInfo           func() map[string]any
	customResolver        security.ResolverFunc
	allowLoopbackForTests bool
}

func newGojaPluginRuntime(opts gojaRuntimeOptions) (*gojaPluginRuntime, error) {
	entryPath := filepath.Join(opts.pluginDir, filepath.FromSlash(opts.manifest.Entry))
	realPluginDir, err := filepath.EvalSymlinks(opts.pluginDir)
	if err != nil {
		return nil, fmt.Errorf("plugin %q: resolve plugin dir: %w", opts.manifest.ID, err)
	}
	realEntry, err := filepath.EvalSymlinks(entryPath)
	if err != nil {
		return nil, fmt.Errorf("plugin %q: resolve entry %q: %w", opts.manifest.ID, opts.manifest.Entry, err)
	}
	if !isSubPath(realPluginDir, realEntry) {
		return nil, fmt.Errorf("plugin %q: entry %q escapes plugin directory", opts.manifest.ID, opts.manifest.Entry)
	}
	srcBytes, err := os.ReadFile(realEntry)
	if err != nil {
		return nil, fmt.Errorf("plugin %q: read entry %q: %w", opts.manifest.ID, opts.manifest.Entry, err)
	}
	prog, err := compileJSProgram(opts.manifest.Entry, string(srcBytes))
	if err != nil {
		return nil, fmt.Errorf("plugin %q: compile %q: %w", opts.manifest.ID, opts.manifest.Entry, err)
	}

	localDicts := loadPluginLocales(filepath.Join(realPluginDir, "locales"))

	var httpClient *http.Client
	allowedHosts := opts.manifest.EffectiveAllowedHosts()
	if len(allowedHosts) > 0 {
		dialer := &security.OutboundSafeDialer{
			AllowedHosts:          allowedHosts,
			Timeout:               4 * time.Second,
			Resolver:              opts.customResolver,
			AllowLoopbackForTests: opts.allowLoopbackForTests,
		}
		httpClient = dialer.NewHTTPClient(5 * time.Second)
	}

	rt := &gojaPluginRuntime{
		manifest:     opts.manifest,
		pluginDir:    realPluginDir,
		program:      prog,
		localI18n:    localDicts,
		kv:           opts.kv,
		logger:       opts.logger,
		hostI18n:     opts.hostI18n,
		getConfig:    opts.getConfig,
		unsealSecret: opts.unsealSecret,
		getSiteInfo:  opts.getSiteInfo,
		httpClient:   httpClient,
		rateBuckets:  make(map[string]pluginRateBucket),
		pool:         make(chan *pooledVM, defaultVMPoolCap),
		sem:          make(chan struct{}, defaultVMMaxConcurrency),
	}

	// Smoke-initialize one VM immediately so top-level runtime errors or infinite loops fail on load.
	initVM, err := rt.createVM()
	if err != nil {
		return nil, err
	}
	rt.pool <- initVM
	return rt, nil
}

func loadPluginLocales(localesDir string) map[string]map[string]string {
	out := make(map[string]map[string]string)
	entries, err := os.ReadDir(localesDir)
	if err != nil {
		return out
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		raw, rErr := os.ReadFile(filepath.Join(localesDir, ent.Name()))
		if rErr != nil {
			continue
		}
		var tbl map[string]string
		if json.Unmarshal(raw, &tbl) == nil && len(tbl) > 0 {
			code := i18n.NormalizeLocale(strings.TrimSuffix(ent.Name(), ".json"))
			out[code] = tbl
		}
	}
	return out
}

func (r *gojaPluginRuntime) translate(locale string, key string, vars map[string]any) string {
	norm := i18n.NormalizeLocale(locale)
	if norm == "" {
		norm = "zh-CN"
	}
	if tbl, ok := r.localI18n[norm]; ok {
		if v, exists := tbl[key]; exists {
			return applyPlaceholders(v, vars)
		}
	}
	if tbl, ok := r.localI18n["en-US"]; ok {
		if v, exists := tbl[key]; exists {
			return applyPlaceholders(v, vars)
		}
	}
	if r.hostI18n != nil {
		return r.hostI18n.T(norm, key, vars)
	}
	return key
}

func applyPlaceholders(tmpl string, vars map[string]any) string {
	if len(vars) == 0 {
		return tmpl
	}
	pairs := make([]string, 0, len(vars)*2)
	for k, v := range vars {
		pairs = append(pairs, "{"+k+"}", fmt.Sprint(v))
	}
	return strings.NewReplacer(pairs...).Replace(tmpl)
}

func (r *gojaPluginRuntime) resolveRuntimeConfigValue(v any) any {
	if s, ok := v.(string); ok && strings.HasPrefix(s, "v1:") && r.unsealSecret != nil {
		return r.unsealSecret(s)
	}
	return v
}

func (r *gojaPluginRuntime) createVM() (pvm *pooledVM, err error) {
	r.vmCreatedCount.Add(1)
	vm := newJSRuntime()
	vm.SetMaxCallStackSize(defaultVMStackLimit)

	wrapper := &pooledVM{
		vm:                vm,
		hookHandlers:      make(map[string][]*jsFunc),
		directiveHandlers: make(map[string]*jsFunc),
	}

	// Inject safe console object wired to isolated JSONL plugin logger.
	consoleObj := vm.NewObject()
	logFn := func(level string) jsNativeFn {
		return func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			parts := make([]string, len(args))
			for i, a := range args {
				parts[i] = a.toString()
			}
			r.logger.Write(r.manifest.ID, level, "console", strings.Join(parts, " "))
			return jsUndefinedVal, nil
		}
	}
	consoleObj.setVal("log", nativeVal(logFn("info")))
	consoleObj.setVal("info", nativeVal(logFn("info")))
	consoleObj.setVal("warn", nativeVal(logFn("warn")))
	consoleObj.setVal("error", nativeVal(logFn("error")))
	vm.Set("console", consoleObj)

	// Inject CommonJS module.exports container.
	moduleObj := vm.NewObject()
	exportsObj := vm.NewObject()
	moduleObj.setVal("exports", jsValue{kind: valObject, obj: exportsObj})
	vm.Set("module", moduleObj)
	vm.Set("exports", exportsObj)

	// Inject capability-gated `volo` (and alias `ctx`) host bridge.
	voloObj := vm.NewObject()
	voloObj.Set("pluginId", r.manifest.ID)
	voloObj.Set("version", r.manifest.Version)
	voloObj.Set("escapeHTML", func(raw string) string {
		return html.EscapeString(raw)
	})
	voloObj.setVal("on", nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if wrapper.hooksFrozen {
			return rt.throwValErr("TypeError", "volo.on can only be called during top-level module initialization")
		}
		if len(args) < 2 {
			return rt.throwValErr("TypeError", "volo.on requires (hookName, handlerFn)")
		}
		hookName := strings.TrimSpace(args[0].toString())
		if hookName == "" || args[1].kind != valFunc || args[1].fn == nil {
			return rt.throwValErr("TypeError", "volo.on requires a non-empty hook name and function handler")
		}
		wrapper.hookHandlers[hookName] = append(wrapper.hookHandlers[hookName], args[1].fn)
		return jsUndefinedVal, nil
	}))

	if r.manifest.HasRenderCapability() {
		voloObj.setVal("registerDirective", nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if wrapper.hooksFrozen {
				return rt.throwValErr("TypeError", "volo.registerDirective can only be called during top-level module initialization")
			}
			if len(args) < 2 {
				return rt.throwValErr("TypeError", "volo.registerDirective requires (name, handlerFn)")
			}
			dirName := strings.ToLower(strings.TrimSpace(args[0].toString()))
			if dirName == "" || dirName == "lock" || args[1].kind != valFunc || args[1].fn == nil {
				return rt.throwValErr("TypeError", "volo.registerDirective requires a valid directive name and function handler")
			}
			wrapper.directiveHandlers[dirName] = args[1].fn
			return jsUndefinedVal, nil
		}))
	}

	if r.manifest.HasFederationCapability() {
		regFedFn := nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if wrapper.hooksFrozen {
				return rt.throwValErr("TypeError", "volo.registerFederation can only be called during top-level module initialization")
			}
			if len(args) < 1 {
				return rt.throwValErr("TypeError", "volo.registerFederation requires a provider object or function")
			}
			if args[0].kind == valFunc && args[0].fn != nil {
				wrapper.federationPublish = args[0].fn
				wrapper.federationDeliver = args[0].fn
				return jsUndefinedVal, nil
			}
			if args[0].kind == valObject && args[0].obj != nil {
				if pub := args[0].obj.get("publish"); pub.kind == valFunc && pub.fn != nil {
					wrapper.federationPublish = pub.fn
				}
				if deliv := args[0].obj.get("deliver"); deliv.kind == valFunc && deliv.fn != nil {
					wrapper.federationDeliver = deliv.fn
				}
				if wrapper.federationPublish == nil && wrapper.federationDeliver == nil {
					return rt.throwValErr("TypeError", "federation provider must define publish or deliver function")
				}
				return jsUndefinedVal, nil
			}
			return rt.throwValErr("TypeError", "volo.registerFederation requires a provider object or function")
		})
		voloObj.setVal("registerFederation", regFedFn)
	}

	cfgObj := vm.NewObject()
	cfgObj.Set("get", func(key string) any {
		cfg := r.getConfig()
		if cfg == nil {
			return nil
		}
		return r.resolveRuntimeConfigValue(cfg[key])
	})
	cfgObj.Set("all", func() map[string]any {
		cfg := r.getConfig()
		out := make(map[string]any, len(cfg))
		for k, v := range cfg {
			out[k] = r.resolveRuntimeConfigValue(v)
		}
		return out
	})
	voloObj.Set("config", cfgObj)

	logBridge := vm.NewObject()
	logBridge.setVal("info", nativeVal(logFn("info")))
	logBridge.setVal("warn", nativeVal(logFn("warn")))
	logBridge.setVal("error", nativeVal(logFn("error")))
	voloObj.Set("log", logBridge)

	i18nBridge := vm.NewObject()
	i18nBridge.Set("t", func(locale string, key string, vars map[string]any) string {
		return r.translate(locale, key, vars)
	})
	voloObj.Set("i18n", i18nBridge)

	if r.manifest.HasPermission("crypto") {
		cryptoBridge := vm.NewObject()
		cryptoBridge.Set("sha256", func(input string) string {
			sum := sha256.Sum256([]byte(input))
			return hex.EncodeToString(sum[:])
		})
		cryptoBridge.Set("hmacSha256", func(secret string, msg string) string {
			mac := hmac.New(sha256.New, []byte(secret))
			_, _ = mac.Write([]byte(msg))
			return hex.EncodeToString(mac.Sum(nil))
		})
		cryptoBridge.Set("randomUUID", func() (string, error) {
			var b [16]byte
			if _, err := rand.Read(b[:]); err != nil {
				return "", err
			}
			b[6] = (b[6] & 0x0f) | 0x40
			b[8] = (b[8] & 0x3f) | 0x80
			return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
		})
		voloObj.Set("crypto", cryptoBridge)
	}

	if r.manifest.HasPermission("site:read") {
		siteBridge := vm.NewObject()
		siteBridge.Set("info", func() map[string]any {
			if r.getSiteInfo == nil {
				return map[string]any{}
			}
			info := r.getSiteInfo()
			if info == nil {
				return map[string]any{}
			}
			out := make(map[string]any, len(info))
			for k, v := range info {
				out[k] = v
			}
			return out
		})
		voloObj.Set("site", siteBridge)
	}

	if r.manifest.HasPermission("ratelimit") || r.manifest.HasPermission("http:routes") {
		rlNative := func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			if len(args) < 2 {
				return jsFalseVal, nil
			}
			windowSec := 60
			if len(args) >= 3 && args[2].kind != valUndefined && args[2].kind != valNull {
				windowSec = int(args[2].toNumber())
			}
			if r.checkRateLimit(args[0].toString(), int(args[1].toNumber()), windowSec) {
				return jsTrueVal, nil
			}
			return jsFalseVal, nil
		}
		voloObj.setVal("rateLimit", jsValue{
			kind: valFunc,
			fn: &jsFunc{
				name:   "rateLimit",
				native: rlNative,
				props: map[string]jsValue{
					"allow": nativeVal(rlNative),
				},
			},
		})
	}

	// Capability 1: plugin_kv (ONLY injected if manifest declares kv / kv:rw / kv:read).
	if r.manifest.CanReadKV() && r.kv != nil {
		kvBridge := vm.NewObject()
		kvBridge.Set("get", func(key string) (any, error) {
			val, ok, gErr := r.kv.Get(wrapper.activeCtx(), key)
			if gErr != nil {
				return nil, gErr
			}
			if !ok {
				return nil, nil
			}
			return val, nil
		})
		kvBridge.Set("getInt", func(key string) (int64, error) {
			val, ok, gErr := r.kv.Get(wrapper.activeCtx(), key)
			if gErr != nil || !ok {
				return 0, gErr
			}
			n, _ := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
			return n, nil
		})
		kvBridge.Set("list", func(prefix string, limit int, afterKey string) ([]KVItem, error) {
			return r.kv.ListPrefix(wrapper.activeCtx(), prefix, limit, afterKey)
		})
		if r.manifest.CanWriteKV() {
			kvBridge.Set("set", func(key string, value string, ttlSec int64) (bool, error) {
				if sErr := r.kv.Set(wrapper.activeCtx(), key, value, ttlSec); sErr != nil {
					return false, sErr
				}
				return true, nil
			})
			delFn := func(key string) (bool, error) {
				if dErr := r.kv.Delete(wrapper.activeCtx(), key); dErr != nil {
					return false, dErr
				}
				return true, nil
			}
			kvBridge.Set("del", delFn)
			kvBridge.Set("delete", delFn)
			kvBridge.Set("incr", func(key string, delta int64, ttlSec int64) (int64, error) {
				if delta == 0 {
					delta = 1
				}
				return r.kv.IncrBy(wrapper.activeCtx(), key, delta, ttlSec)
			})
			kvBridge.Set("clear", func() (bool, error) {
				if cErr := ClearPluginKV(wrapper.activeCtx(), r.kv.store, r.manifest.ID); cErr != nil {
					return false, cErr
				}
				return true, nil
			})
		}
		voloObj.Set("kv", kvBridge)
	}

	// Sandboxed read-only package asset reader (strictly confined to r.pluginDir, <= 2 MiB, e.g. lexicon dictionaries).
	if r.pluginDir != "" {
		readTextFn := func(relPath string) (string, error) {
			raw, rErr := r.readPluginAsset(relPath)
			if rErr != nil {
				return "", rErr
			}
			return string(raw), nil
		}
		assetsBridge := vm.NewObject()
		assetsBridge.Set("readText", readTextFn)
		assetsBridge.Set("readJSON", func(relPath string) (any, error) {
			raw, rErr := r.readPluginAsset(relPath)
			if rErr != nil {
				return nil, rErr
			}
			var parsed any
			if uErr := json.Unmarshal(raw, &parsed); uErr != nil {
				return nil, fmt.Errorf("plugin asset %q: invalid JSON: %w", relPath, uErr)
			}
			return parsed, nil
		})
		assetsBridge.Set("exists", func(relPath string) bool {
			_, rErr := r.resolvePluginAssetPath(relPath)
			return rErr == nil
		})
		voloObj.Set("readAsset", readTextFn)
		voloObj.Set("assets", assetsBridge)
	}

	// Capability 2: Outbound SSRF-safe HTTP client (ONLY injected if allowed_hosts / http.outbound:<host> declared).
	if r.httpClient != nil {
		httpBridge := vm.NewObject()
		httpBridge.Set("fetch", func(rawURL string, opts map[string]any) (map[string]any, error) {
			return r.executeFetch(wrapper.activeCtx(), rawURL, opts)
		})
		voloObj.Set("http", httpBridge)
	}

	vm.Set("volo", voloObj)
	vm.Set("ctx", voloObj)

	// Guard top-level module evaluation with a strict 500ms timeout and panic recovery.
	timer := time.AfterFunc(500*time.Millisecond, func() {
		vm.Interrupt(ErrExecutionTimeout)
	})
	defer func() {
		timer.Stop()
		if rec := recover(); rec != nil {
			err = fmt.Errorf("plugin %q: panic during init: %v", r.manifest.ID, rec)
			pvm = nil
		}
	}()

	if _, runErr := vm.RunProgram(r.program); runErr != nil {
		return nil, normalizeJSError(r.manifest.ID, "init", runErr)
	}
	wrapper.hooksFrozen = true

	modVal := moduleObj.get("exports")
	if modVal.kind == valObject && modVal.obj != nil {
		wrapper.exports = modVal.obj
	} else {
		wrapper.exports = exportsObj
	}
	return wrapper, nil
}

func (p *pooledVM) activeCtx() context.Context {
	if p.reqCtx != nil {
		return p.reqCtx
	}
	return context.Background()
}

func (r *gojaPluginRuntime) executeFetch(ctx context.Context, rawURL string, opts map[string]any) (map[string]any, error) {
	method := http.MethodGet
	var bodyReader io.Reader
	headers := make(map[string]string)

	if opts != nil {
		if m, ok := opts["method"].(string); ok && strings.TrimSpace(m) != "" {
			method = strings.ToUpper(strings.TrimSpace(m))
		}
		if b, ok := opts["body"].(string); ok && b != "" {
			if len(b) > maxPluginFetchBodyBytes {
				return nil, errors.New("plugin http.fetch: request body exceeds 1 MiB")
			}
			bodyReader = bytes.NewBufferString(b)
		}
		if rawHdrs, ok := opts["headers"].(map[string]any); ok {
			for k, v := range rawHdrs {
				headers[k] = fmt.Sprint(v)
			}
		}
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead:
	default:
		return nil, fmt.Errorf("plugin http.fetch: unsupported HTTP method %q", method)
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Volo-go-Plugin/"+r.manifest.ID)
	for k, v := range headers {
		if strings.EqualFold(k, "Host") {
			continue
		}
		req.Header.Set(k, v)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxPluginFetchBodyBytes))
	if err != nil {
		return nil, err
	}
	respHeaders := make(map[string]string, len(resp.Header))
	for k, vs := range resp.Header {
		if len(vs) > 0 {
			respHeaders[strings.ToLower(k)] = vs[0]
		}
	}
	bodyText := string(bodyBytes)
	return map[string]any{
		"status":  resp.StatusCode,
		"ok":      resp.StatusCode >= 200 && resp.StatusCode < 300,
		"headers": respHeaders,
		"body":    bodyText,
	}, nil
}

// hasCallable reports whether fnName is defined on module.exports or global scope in the plugin runtime.
func (r *gojaPluginRuntime) hasCallable(fnName string) bool {
	if r == nil {
		return false
	}
	var pvm *pooledVM
	select {
	case pvm = <-r.pool:
	default:
		created, err := r.createVM()
		if err != nil {
			return false
		}
		pvm = created
	}
	defer func() {
		if pvm.uses < maxPooledVMUses {
			select {
			case r.pool <- pvm:
			default:
			}
		}
	}()
	_, ok := resolveCallable(pvm.vm, pvm.exports, fnName)
	return ok
}

func (r *gojaPluginRuntime) withPooledVM(
	ctx context.Context,
	timeout time.Duration,
	stage string,
	run func(pvm *pooledVM) (out any, found bool, tainted bool, callErr error),
) (out any, found bool, err error) {
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}

	var pvm *pooledVM
	select {
	case pvm = <-r.pool:
	default:
		created, cErr := r.createVM()
		if cErr != nil {
			return nil, false, cErr
		}
		pvm = created
	}
	pvm.uses++

	tainted := false
	defer func() {
		if rec := recover(); rec != nil {
			tainted = true
			err = fmt.Errorf("plugin %q: panic in %s: %v", r.manifest.ID, stage, rec)
		}
		pvm.reqCtx = nil
		if !tainted && err == nil && pvm.uses < maxPooledVMUses {
			select {
			case r.pool <- pvm:
			default:
			}
		}
	}()

	vm := pvm.vm
	vm.ClearInterrupt()
	pvm.reqCtx = ctx

	var intMu sync.Mutex
	finished := false
	interruptVM := func(cause error) {
		intMu.Lock()
		if !finished {
			vm.Interrupt(cause)
		}
		intMu.Unlock()
	}

	done := make(chan struct{})
	timer := time.AfterFunc(timeout, func() {
		interruptVM(ErrExecutionTimeout)
	})
	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				interruptVM(ctx.Err())
			case <-done:
			}
		}()
	}
	defer func() {
		timer.Stop()
		intMu.Lock()
		finished = true
		intMu.Unlock()
		close(done)
	}()

	out, found, tainted, err = run(pvm)
	return out, found, err
}

// invoke borrows or creates a sandboxed JS VM, calls fnName (from module.exports or global scope,
// plus registered volo.on(hookName, fn) listeners when fnName == "onHook") under strict timeout +
// stack limit + panic recovery, and recycles the VM only if healthy and uses < 1000.
func (r *gojaPluginRuntime) invoke(ctx context.Context, timeout time.Duration, fnName string, args ...any) (out any, found bool, err error) {
	return r.withPooledVM(ctx, timeout, fnName, func(pvm *pooledVM) (any, bool, bool, error) {
		vm := pvm.vm
		callable, ok := resolveCallable(vm, pvm.exports, fnName)
		var extraHookHandlers []*jsFunc
		var hookName string
		if fnName == "onHook" && len(args) >= 1 {
			if hn, hOk := args[0].(string); hOk && hn != "" {
				hookName = hn
				if pvm.exports != nil {
					if hooksVal := pvm.exports.get("hooks"); hooksVal.kind == valObject && hooksVal.obj != nil {
						if hFn := hooksVal.obj.get(hn); hFn.kind == valFunc && hFn.fn != nil {
							extraHookHandlers = append(extraHookHandlers, hFn.fn)
						}
					}
				}
				if len(pvm.hookHandlers) > 0 {
					if hs := pvm.hookHandlers[hn]; len(hs) > 0 {
						extraHookHandlers = append(extraHookHandlers, hs...)
					}
					if star := pvm.hookHandlers["*"]; len(star) > 0 {
						extraHookHandlers = append(extraHookHandlers, star...)
					}
				}
			}
		}
		if !ok && len(extraHookHandlers) == 0 {
			return nil, false, false, nil
		}

		var lastRes = jsUndefinedVal
		var curPayloadVal = jsUndefinedVal
		if len(args) >= 2 {
			curPayloadVal = vm.ToValue(args[1])
		}

		if ok {
			jsArgs := make([]jsValue, len(args))
			for i, a := range args {
				jsArgs[i] = vm.ToValue(a)
			}
			res, callErr := vm.callFunc(callable, jsUndefinedVal, jsArgs)
			if callErr != nil {
				return nil, true, true, normalizeJSError(r.manifest.ID, fnName, callErr)
			}
			if res.kind != valUndefined && res.kind != valNull {
				lastRes = res
				if fnName == "onHook" {
					curPayloadVal = res
				}
			}
		}

		for _, handlerFn := range extraHookHandlers {
			hArgs := []jsValue{curPayloadVal, {kind: valString, str: hookName}}
			res, callErr := vm.callFunc(handlerFn, jsUndefinedVal, hArgs)
			if callErr != nil {
				return nil, true, true, normalizeJSError(r.manifest.ID, fnName+":"+hookName, callErr)
			}
			if res.kind != valUndefined && res.kind != valNull {
				lastRes = res
				curPayloadVal = res
			}
		}

		if lastRes.kind == valUndefined || lastRes.kind == valNull {
			return nil, true, false, nil
		}
		return lastRes.Export(), true, false, nil
	})
}

// invokeDirective invokes a registered custom markdown directive handler (`volo.registerDirective` or `hooks["render.directive"]`).
func (r *gojaPluginRuntime) invokeDirective(
	ctx context.Context,
	timeout time.Duration,
	name string,
	attrs map[string]string,
	body string,
) (string, bool, error) {
	normName := strings.ToLower(strings.TrimSpace(name))
	if normName == "" || normName == "lock" {
		return "", false, nil
	}
	raw, found, err := r.withPooledVM(ctx, timeout, "directive:"+normName, func(pvm *pooledVM) (any, bool, bool, error) {
		vm := pvm.vm
		var handlers []*jsFunc
		if fn := pvm.directiveHandlers[normName]; fn != nil {
			handlers = append(handlers, fn)
		}
		if len(handlers) == 0 && pvm.exports != nil {
			if dirObj := pvm.exports.get("directives"); dirObj.kind == valObject && dirObj.obj != nil {
				if fnVal := dirObj.obj.get(normName); fnVal.kind == valFunc && fnVal.fn != nil {
					handlers = append(handlers, fnVal.fn)
				}
			}
			if hooksObj := pvm.exports.get("hooks"); hooksObj.kind == valObject && hooksObj.obj != nil {
				if fnVal := hooksObj.obj.get(HookRenderDirective); fnVal.kind == valFunc && fnVal.fn != nil {
					handlers = append(handlers, fnVal.fn)
				}
			}
		}
		if len(handlers) == 0 {
			if hs := pvm.hookHandlers[HookRenderDirective]; len(hs) > 0 {
				handlers = append(handlers, hs...)
			}
		}
		var onHookFn *jsFunc
		if len(handlers) == 0 {
			if fn, ok := resolveCallable(vm, pvm.exports, "renderDirective"); ok {
				handlers = append(handlers, fn)
			} else if fn, ok := resolveCallable(vm, pvm.exports, "onHook"); ok {
				onHookFn = fn
			}
		}
		if len(handlers) == 0 && onHookFn == nil {
			return nil, false, false, nil
		}

		attrsCopy := make(map[string]string, len(attrs))
		for k, v := range attrs {
			attrsCopy[k] = v
		}
		ctxArg := vm.ToValue(map[string]any{
			"name":  normName,
			"attrs": attrsCopy,
			"body":  body,
		})

		if onHookFn != nil {
			res, callErr := vm.callFunc(onHookFn, jsUndefinedVal, []jsValue{{kind: valString, str: HookRenderDirective}, ctxArg})
			if callErr != nil {
				return nil, true, true, normalizeJSError(r.manifest.ID, "directive:"+normName, callErr)
			}
			if res.kind == valUndefined || res.kind == valNull {
				return nil, false, false, nil
			}
			return res.Export(), true, false, nil
		}

		for _, fn := range handlers {
			res, callErr := vm.callFunc(fn, jsUndefinedVal, []jsValue{ctxArg})
			if callErr != nil {
				return nil, true, true, normalizeJSError(r.manifest.ID, "directive:"+normName, callErr)
			}
			if res.kind != valUndefined && res.kind != valNull {
				return res.Export(), true, false, nil
			}
		}
		return nil, false, false, nil
	})
	if err != nil {
		return "", false, err
	}
	if !found || raw == nil {
		return "", false, nil
	}
	switch v := raw.(type) {
	case string:
		return v, true, nil
	case map[string]any:
		if h, ok := v["html"].(string); ok {
			return h, true, nil
		}
	}
	return "", false, nil
}

// invokeFederation invokes a federation provider's publish / deliver handler under strict timeout and panic recovery.
func (r *gojaPluginRuntime) invokeFederation(
	ctx context.Context,
	timeout time.Duration,
	action string,
	articlePayload map[string]any,
) (any, bool, error) {
	normAction := strings.ToLower(strings.TrimSpace(action))
	if normAction == "" {
		normAction = "publish"
	}
	return r.withPooledVM(ctx, timeout, "federation:"+normAction, func(pvm *pooledVM) (any, bool, bool, error) {
		vm := pvm.vm
		var targetFn *jsFunc
		wantDeliver := normAction == "deliver"

		if wantDeliver {
			targetFn = pvm.federationDeliver
			if targetFn == nil {
				targetFn = pvm.federationPublish
			}
		} else {
			targetFn = pvm.federationPublish
			if targetFn == nil {
				targetFn = pvm.federationDeliver
			}
		}

		if targetFn == nil && pvm.exports != nil {
			if fedObj := pvm.exports.get("federation"); fedObj.kind == valObject && fedObj.obj != nil {
				primary, secondary := "publish", "deliver"
				if wantDeliver {
					primary, secondary = "deliver", "publish"
				}
				if fnVal := fedObj.obj.get(primary); fnVal.kind == valFunc && fnVal.fn != nil {
					targetFn = fnVal.fn
				} else if fnVal := fedObj.obj.get(secondary); fnVal.kind == valFunc && fnVal.fn != nil {
					targetFn = fnVal.fn
				}
			}
		}
		if targetFn == nil {
			candidates := []string{"publish", "deliver", "federationPublish", "federationDeliver", "publishFederation", "deliverFederation"}
			if wantDeliver {
				candidates = []string{"deliver", "publish", "federationDeliver", "federationPublish", "deliverFederation", "publishFederation"}
			}
			for _, cand := range candidates {
				if fn, ok := resolveCallable(vm, pvm.exports, cand); ok {
					targetFn = fn
					break
				}
			}
		}
		if targetFn == nil && pvm.exports != nil {
			if hooksObj := pvm.exports.get("hooks"); hooksObj.kind == valObject && hooksObj.obj != nil {
				for _, hk := range []string{"federation." + normAction, "federation.publish", "federation.deliver"} {
					if fnVal := hooksObj.obj.get(hk); fnVal.kind == valFunc && fnVal.fn != nil {
						targetFn = fnVal.fn
						break
					}
				}
			}
		}
		if targetFn == nil && len(pvm.hookHandlers) > 0 {
			for _, hk := range []string{"federation." + normAction, "federation.publish", "federation.deliver", "federation.*"} {
				if hs := pvm.hookHandlers[hk]; len(hs) > 0 {
					targetFn = hs[0]
					break
				}
			}
		}

		ev := make(map[string]any, len(articlePayload)+2)
		for k, v := range articlePayload {
			ev[k] = v
		}
		ev["action"] = normAction
		if _, hasArticle := ev["article"]; !hasArticle {
			ev["article"] = articlePayload
		}
		jsEv := vm.ToValue(ev)
		jsAct := jsValue{kind: valString, str: normAction}

		if targetFn != nil {
			res, callErr := vm.callFunc(targetFn, jsUndefinedVal, []jsValue{jsEv, jsAct})
			if callErr != nil {
				return nil, true, true, normalizeJSError(r.manifest.ID, "federation:"+normAction, callErr)
			}
			if res.kind == valUndefined || res.kind == valNull {
				return nil, true, false, nil
			}
			return res.Export(), true, false, nil
		}

		if onHookFn, ok := resolveCallable(vm, pvm.exports, "onHook"); ok {
			res, callErr := vm.callFunc(onHookFn, jsUndefinedVal, []jsValue{{kind: valString, str: "federation." + normAction}, jsEv})
			if callErr != nil {
				return nil, true, true, normalizeJSError(r.manifest.ID, "federation:"+normAction, callErr)
			}
			if res.kind == valUndefined || res.kind == valNull {
				return nil, true, false, nil
			}
			return res.Export(), true, false, nil
		}
		return nil, false, false, nil
	})
}

func resolveCallable(vm *jsRuntime, exports *jsObject, fnName string) (*jsFunc, bool) {
	if exports != nil {
		if v := exports.get(fnName); v.kind == valFunc && v.fn != nil {
			return v.fn, true
		}
	}
	if v := vm.Get(fnName); v.kind == valFunc && v.fn != nil {
		return v.fn, true
	}
	return nil, false
}

func normalizeJSError(pluginID string, stage string, err error) error {
	var interrupted *jsInterruptedError
	if errors.As(err, &interrupted) {
		valStr := fmt.Sprint(interrupted.Value())
		if strings.Contains(valStr, ErrExecutionTimeout.Error()) {
			return fmt.Errorf("plugin %q (%s): %w", pluginID, stage, ErrExecutionTimeout)
		}
		return fmt.Errorf("plugin %q (%s): interrupted: %s", pluginID, stage, valStr)
	}
	return fmt.Errorf("plugin %q (%s): %w", pluginID, stage, err)
}

func (r *gojaPluginRuntime) checkRateLimit(key string, maxCount int, windowSec int) bool {
	if maxCount <= 0 {
		return false
	}
	if windowSec <= 0 {
		windowSec = 60
	}
	if windowSec > 86400 {
		windowSec = 86400
	}
	key = strings.TrimSpace(key)
	if key == "" {
		key = "default"
	}
	if len(key) > 256 {
		key = key[:256]
	}
	nowUnix := time.Now().Unix()
	winSec := int64(windowSec)
	windowID := nowUnix / winSec

	r.rateMu.Lock()
	defer r.rateMu.Unlock()

	if len(r.rateBuckets) >= maxPluginRateLimitKeys {
		for k, b := range r.rateBuckets {
			if b.expiresUnix <= nowUnix {
				delete(r.rateBuckets, k)
			}
		}
		for len(r.rateBuckets) >= maxPluginRateLimitKeys {
			for k := range r.rateBuckets {
				delete(r.rateBuckets, k)
				break
			}
		}
	}

	b := r.rateBuckets[key]
	if b.windowID != windowID || b.expiresUnix <= nowUnix {
		b = pluginRateBucket{
			windowID:    windowID,
			expiresUnix: (windowID + 1) * winSec,
			count:       0,
		}
	}
	if b.count >= maxCount {
		r.rateBuckets[key] = b
		return false
	}
	b.count++
	r.rateBuckets[key] = b
	return true
}

const maxPluginAssetReadBytes = 2 << 20 // 2 MiB cap for bundled lexicon/asset files

func (r *gojaPluginRuntime) resolvePluginAssetPath(relPath string) (string, error) {
	cleanRel := strings.TrimSpace(relPath)
	if cleanRel == "" || strings.ContainsRune(cleanRel, '\x00') {
		return "", errors.New("plugin asset: empty or invalid path")
	}
	slashPath := filepath.FromSlash(strings.TrimPrefix(cleanRel, "./"))
	if filepath.IsAbs(slashPath) || strings.HasPrefix(slashPath, "..") {
		return "", fmt.Errorf("plugin asset %q: path escapes plugin directory", relPath)
	}
	joined := filepath.Join(r.pluginDir, slashPath)
	realTarget, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", fmt.Errorf("plugin asset %q: %w", relPath, err)
	}
	if realTarget == r.pluginDir || !isSubPath(r.pluginDir, realTarget) {
		return "", fmt.Errorf("plugin asset %q: path escapes plugin directory", relPath)
	}
	st, err := os.Stat(realTarget)
	if err != nil {
		return "", fmt.Errorf("plugin asset %q: %w", relPath, err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("plugin asset %q: not a regular file", relPath)
	}
	if st.Size() > maxPluginAssetReadBytes {
		return "", fmt.Errorf("plugin asset %q: exceeds %d byte limit", relPath, maxPluginAssetReadBytes)
	}
	return realTarget, nil
}

func (r *gojaPluginRuntime) readPluginAsset(relPath string) ([]byte, error) {
	realTarget, err := r.resolvePluginAssetPath(relPath)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(realTarget)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxPluginAssetReadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxPluginAssetReadBytes {
		return nil, fmt.Errorf("plugin asset %q: exceeds %d byte limit", relPath, maxPluginAssetReadBytes)
	}
	return raw, nil
}

func isSubPath(rootReal string, targetReal string) bool {
	rootClean := filepath.Clean(rootReal)
	targetClean := filepath.Clean(targetReal)
	if targetClean == rootClean {
		return true
	}
	return strings.HasPrefix(targetClean, rootClean+string(filepath.Separator))
}
