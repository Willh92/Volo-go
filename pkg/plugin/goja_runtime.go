package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dop251/goja"

	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/security"
)

const (
	defaultVMStackLimit     = 512
	defaultVMPoolCap        = 4
	defaultVMMaxConcurrency = 8
	maxPluginFetchBodyBytes = 1 << 20 // 1 MiB cap on outbound HTTP responses
)

// ErrExecutionTimeout is returned when a plugin hook, route, slot, or cron exceeds its time budget.
var ErrExecutionTimeout = errors.New("plugin: execution interrupted due to timeout")

type pooledVM struct {
	vm      *goja.Runtime
	exports *goja.Object
	reqCtx  context.Context
}

type gojaPluginRuntime struct {
	manifest   Manifest
	pluginDir  string
	program    *goja.Program
	localI18n  map[string]map[string]string
	kv         *KVStore
	logger     *PluginLogger
	hostI18n   *i18n.Catalog
	getConfig  func() map[string]any
	httpClient *http.Client

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
	prog, err := goja.Compile(opts.manifest.Entry, string(srcBytes), true)
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
		manifest:   opts.manifest,
		pluginDir:  realPluginDir,
		program:    prog,
		localI18n:  localDicts,
		kv:         opts.kv,
		logger:     opts.logger,
		hostI18n:   opts.hostI18n,
		getConfig:  opts.getConfig,
		httpClient: httpClient,
		pool:       make(chan *pooledVM, defaultVMPoolCap),
		sem:        make(chan struct{}, defaultVMMaxConcurrency),
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

func (r *gojaPluginRuntime) createVM() (pvm *pooledVM, err error) {
	vm := goja.New()
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
	vm.SetMaxCallStackSize(defaultVMStackLimit)

	wrapper := &pooledVM{vm: vm}

	// Inject safe console object wired to isolated JSONL plugin logger.
	consoleObj := vm.NewObject()
	logFn := func(level string) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			parts := make([]string, len(call.Arguments))
			for i, a := range call.Arguments {
				parts[i] = a.String()
			}
			r.logger.Write(r.manifest.ID, level, "console", strings.Join(parts, " "))
			return goja.Undefined()
		}
	}
	_ = consoleObj.Set("log", logFn("info"))
	_ = consoleObj.Set("info", logFn("info"))
	_ = consoleObj.Set("warn", logFn("warn"))
	_ = consoleObj.Set("error", logFn("error"))
	_ = vm.Set("console", consoleObj)

	// Inject CommonJS module.exports container.
	moduleObj := vm.NewObject()
	exportsObj := vm.NewObject()
	_ = moduleObj.Set("exports", exportsObj)
	_ = vm.Set("module", moduleObj)
	_ = vm.Set("exports", exportsObj)

	// Inject capability-gated `volo` (and alias `ctx`) host bridge.
	voloObj := vm.NewObject()
	_ = voloObj.Set("pluginId", r.manifest.ID)
	_ = voloObj.Set("version", r.manifest.Version)

	cfgObj := vm.NewObject()
	_ = cfgObj.Set("get", func(key string) any {
		cfg := r.getConfig()
		if cfg == nil {
			return nil
		}
		return cfg[key]
	})
	_ = cfgObj.Set("all", func() map[string]any {
		cfg := r.getConfig()
		out := make(map[string]any, len(cfg))
		for k, v := range cfg {
			out[k] = v
		}
		return out
	})
	_ = voloObj.Set("config", cfgObj)

	logBridge := vm.NewObject()
	_ = logBridge.Set("info", logFn("info"))
	_ = logBridge.Set("warn", logFn("warn"))
	_ = logBridge.Set("error", logFn("error"))
	_ = voloObj.Set("log", logBridge)

	i18nBridge := vm.NewObject()
	_ = i18nBridge.Set("t", func(locale string, key string, vars map[string]any) string {
		return r.translate(locale, key, vars)
	})
	_ = voloObj.Set("i18n", i18nBridge)

	// Capability 1: plugin_kv (ONLY injected if manifest declares kv / kv:rw / kv:read).
	if r.manifest.CanReadKV() && r.kv != nil {
		kvBridge := vm.NewObject()
		_ = kvBridge.Set("get", func(key string) any {
			val, ok, gErr := r.kv.Get(wrapper.activeCtx(), key)
			if gErr != nil {
				panic(vm.NewGoError(gErr))
			}
			if !ok {
				return nil
			}
			return val
		})
		_ = kvBridge.Set("list", func(prefix string, limit int) []KVItem {
			items, lErr := r.kv.ListPrefix(wrapper.activeCtx(), prefix, limit)
			if lErr != nil {
				panic(vm.NewGoError(lErr))
			}
			return items
		})
		if r.manifest.CanWriteKV() {
			_ = kvBridge.Set("set", func(key string, value string, ttlSec int64) {
				if sErr := r.kv.Set(wrapper.activeCtx(), key, value, ttlSec); sErr != nil {
					panic(vm.NewGoError(sErr))
				}
			})
			delFn := func(key string) {
				if dErr := r.kv.Delete(wrapper.activeCtx(), key); dErr != nil {
					panic(vm.NewGoError(dErr))
				}
			}
			_ = kvBridge.Set("del", delFn)
			_ = kvBridge.Set("delete", delFn)
			_ = kvBridge.Set("incr", func(key string, delta int64, ttlSec int64) int64 {
				if delta == 0 {
					delta = 1
				}
				next, iErr := r.kv.IncrBy(wrapper.activeCtx(), key, delta, ttlSec)
				if iErr != nil {
					panic(vm.NewGoError(iErr))
				}
				return next
			})
		}
		_ = voloObj.Set("kv", kvBridge)
	}

	// Capability 2: Outbound SSRF-safe HTTP client (ONLY injected if allowed_hosts / http.outbound:<host> declared).
	if r.httpClient != nil {
		httpBridge := vm.NewObject()
		_ = httpBridge.Set("fetch", func(rawURL string, opts map[string]any) map[string]any {
			res, fErr := r.executeFetch(wrapper.activeCtx(), rawURL, opts)
			if fErr != nil {
				panic(vm.NewGoError(fErr))
			}
			return res
		})
		_ = voloObj.Set("http", httpBridge)
	}

	_ = vm.Set("volo", voloObj)
	_ = vm.Set("ctx", voloObj)

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
		return nil, normalizeGojaError(r.manifest.ID, "init", runErr)
	}

	modVal := moduleObj.Get("exports")
	if modVal != nil && !goja.IsUndefined(modVal) && !goja.IsNull(modVal) {
		wrapper.exports = modVal.ToObject(vm)
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

// invoke borrows or creates a sandboxed goja VM, calls fnName (from module.exports or global scope)
// under strict timeout + stack limit + panic recovery, and recycles the VM only if healthy.
func (r *gojaPluginRuntime) invoke(ctx context.Context, timeout time.Duration, fnName string, args ...any) (out any, found bool, err error) {
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

	tainted := false
	defer func() {
		if rec := recover(); rec != nil {
			tainted = true
			err = fmt.Errorf("plugin %q: panic in %s: %v", r.manifest.ID, fnName, rec)
		}
		pvm.reqCtx = nil
		if !tainted && err == nil {
			select {
			case r.pool <- pvm:
			default:
			}
		}
	}()

	vm := pvm.vm
	vm.ClearInterrupt()
	pvm.reqCtx = ctx

	callable, ok := resolveCallable(vm, pvm.exports, fnName)
	if !ok {
		return nil, false, nil
	}

	jsArgs := make([]goja.Value, len(args))
	for i, a := range args {
		jsArgs[i] = vm.ToValue(a)
	}

	done := make(chan struct{})
	timer := time.AfterFunc(timeout, func() {
		vm.Interrupt(ErrExecutionTimeout)
	})
	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				vm.Interrupt(ctx.Err())
			case <-done:
			}
		}()
	}
	defer func() {
		timer.Stop()
		close(done)
	}()

	res, callErr := callable(goja.Undefined(), jsArgs...)
	if callErr != nil {
		tainted = true
		return nil, true, normalizeGojaError(r.manifest.ID, fnName, callErr)
	}
	if res == nil || goja.IsUndefined(res) || goja.IsNull(res) {
		return nil, true, nil
	}
	return res.Export(), true, nil
}

func resolveCallable(vm *goja.Runtime, exports *goja.Object, fnName string) (goja.Callable, bool) {
	if exports != nil {
		if v := exports.Get(fnName); v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
			if fn, ok := goja.AssertFunction(v); ok {
				return fn, true
			}
		}
	}
	if v := vm.Get(fnName); v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
		if fn, ok := goja.AssertFunction(v); ok {
			return fn, true
		}
	}
	return nil, false
}

func normalizeGojaError(pluginID string, stage string, err error) error {
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		valStr := fmt.Sprint(interrupted.Value())
		if strings.Contains(valStr, ErrExecutionTimeout.Error()) {
			return fmt.Errorf("plugin %q (%s): %w", pluginID, stage, ErrExecutionTimeout)
		}
		return fmt.Errorf("plugin %q (%s): interrupted: %s", pluginID, stage, valStr)
	}
	return fmt.Errorf("plugin %q (%s): %w", pluginID, stage, err)
}

func isSubPath(rootReal string, targetReal string) bool {
	rootClean := filepath.Clean(rootReal)
	targetClean := filepath.Clean(targetReal)
	if targetClean == rootClean {
		return true
	}
	return strings.HasPrefix(targetClean, rootClean+string(filepath.Separator))
}
