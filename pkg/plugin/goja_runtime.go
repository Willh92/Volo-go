package plugin

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Willh92/Volo-go/internal/i18n"
	"github.com/Willh92/Volo-go/internal/security"
	"github.com/Willh92/Volo-go/pkg/wordfilter"
)

const (
	defaultVMStackLimit      = 512
	defaultVMPoolCap         = 4
	defaultVMMaxConcurrency  = 8
	maxPooledVMUses          = 1000
	maxPluginFetchBodyBytes  = 4 << 20  // 4 MiB cap on default text outbound HTTP responses (supports full houbb dict sync)
	maxPluginBinaryBodyBytes = 32 << 20 // 32 MiB cap on binary base64 outbound HTTP request/response bodies
	maxPluginAssetReadBytes  = 4 << 20  // 4 MiB cap on read-only decompressed plugin asset reads
	maxPluginRateLimitKeys   = 2048
	maxCachedWordMatcherSigs = 2
	remoteDictRawChunkBytes  = 36_000 // Encodes to exactly 48,000 base64 chars (< 64 KiB & < 512 KiB plugin_kv cap)
	remoteDictBase64ChunkLen = 48_000
)

// ErrExecutionTimeout is returned when a plugin hook, route, slot, or cron exceeds its time budget.
var ErrExecutionTimeout = errors.New("plugin: execution interrupted due to timeout")

func kvMap(kv ...any) map[string]any {
	m := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			m[k] = kv[i+1]
		}
	}
	return m
}

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
	setNativeBudget   func(time.Duration)
}

type pluginRateBucket struct {
	windowID    int64
	expiresUnix int64
	count       int
}

type runtimeSanitizerConfig struct {
	initialized   bool
	forWordRev    int64
	enabled       bool
	contentSig    string
	nicknameSig   string
	checkNickname bool
}

type gojaPluginRuntime struct {
	manifest              Manifest
	pluginDir             string
	program               *jsProgram
	localI18n             map[string]map[string]string
	kv                    *KVStore
	cron                  *CronRegistry
	logger                *PluginLogger
	hostI18n              *i18n.Catalog
	getConfig             func() map[string]any
	unsealSecret          func(string) string
	getSiteInfo           func() map[string]any
	customResolver        security.ResolverFunc
	allowLoopbackForTests bool
	httpEnabled           bool

	vmCreatedCount atomic.Uint64

	httpMu        sync.Mutex
	httpHostsKey  string
	httpClientPtr *http.Client

	sanitizerWarmMu sync.Mutex
	wordCompileMu   sync.Mutex
	wordMu          sync.RWMutex
	wordMatchers    map[string]*wordfilter.Matcher
	wordOrder       []string
	sanitizerCfg    runtimeSanitizerConfig
	wordRev         atomic.Int64

	rateMu      sync.Mutex
	rateBuckets map[string]pluginRateBucket

	pool chan *pooledVM
	sem  chan struct{}
}

type gojaRuntimeOptions struct {
	manifest              Manifest
	pluginDir             string
	kv                    *KVStore
	cron                  *CronRegistry
	logger                *PluginLogger
	hostI18n              *i18n.Catalog
	getConfig             func() map[string]any
	unsealSecret          func(string) string
	getSiteInfo           func() map[string]any
	customResolver        security.ResolverFunc
	allowLoopbackForTests bool
}

func manifestHasURLConfigField(m Manifest) bool {
	for _, f := range m.ConfigSchema {
		if f.Type == "url" {
			return true
		}
	}
	return false
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

	allowedHosts := opts.manifest.EffectiveAllowedHosts()
	httpEnabled := len(allowedHosts) > 0 ||
		((opts.manifest.HasPermission("http:outbound") || opts.manifest.HasPermission("net:http")) && manifestHasURLConfigField(opts.manifest))

	rt := &gojaPluginRuntime{
		manifest:              opts.manifest,
		pluginDir:             realPluginDir,
		program:               prog,
		localI18n:             localDicts,
		kv:                    opts.kv,
		cron:                  opts.cron,
		logger:                opts.logger,
		hostI18n:              opts.hostI18n,
		getConfig:             opts.getConfig,
		unsealSecret:          opts.unsealSecret,
		getSiteInfo:           opts.getSiteInfo,
		customResolver:        opts.customResolver,
		allowLoopbackForTests: opts.allowLoopbackForTests,
		httpEnabled:           httpEnabled,
		wordMatchers:          make(map[string]*wordfilter.Matcher, maxCachedWordMatcherSigs),
		rateBuckets:           make(map[string]pluginRateBucket),
		pool:                  make(chan *pooledVM, defaultVMPoolCap),
		sem:                   make(chan struct{}, defaultVMMaxConcurrency),
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
	voloObj.setVal("reject", nativeVal(func(rt *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		reason := ""
		if len(args) >= 1 && args[0].kind != valUndefined && args[0].kind != valNull {
			reason = strings.TrimSpace(args[0].toString())
		}
		code := ""
		if len(args) >= 2 && args[1].kind != valUndefined && args[1].kind != valNull {
			code = strings.TrimSpace(args[1].toString())
		}
		rejObj := rt.NewObject()
		rejObj.setVal("__volo_rejected__", jsTrueVal)
		rejObj.setVal("reject", jsTrueVal)
		rejObj.setVal("allow", jsFalseVal)
		rejObj.setVal("reason", jsValue{kind: valString, str: reason})
		if code != "" {
			rejObj.setVal("code", jsValue{kind: valString, str: code})
		}
		return jsValue{kind: valObject, obj: rejObj}, nil
	}))
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
			incrBufFn := func(key string, delta int64) (int64, error) {
				if delta == 0 {
					delta = 1
				}
				return r.kv.IncrBuffered(wrapper.activeCtx(), key, delta)
			}
			kvBridge.Set("incrAsync", incrBufFn)
			kvBridge.Set("incrBuffered", incrBufFn)
			kvBridge.Set("flush", func() (bool, error) {
				if fErr := r.kv.Flush(wrapper.activeCtx()); fErr != nil {
					return false, fErr
				}
				return true, nil
			})
			kvBridge.Set("deletePrefix", func(prefix string) (int64, error) {
				n, dErr := r.kv.DeletePrefix(wrapper.activeCtx(), prefix)
				if dErr != nil {
					return 0, dErr
				}
				if n > 0 {
					r.clearWordMatchers()
				}
				return n, nil
			})
			kvBridge.Set("clear", func() (bool, error) {
				if cErr := ClearPluginKV(wrapper.activeCtx(), r.kv.store, r.manifest.ID); cErr != nil {
					return false, cErr
				}
				r.clearWordMatchers()
				return true, nil
			})
		}
		voloObj.Set("kv", kvBridge)
	}

	// Sandboxed read-only package asset bridge confined strictly to r.pluginDir (supports transparent .gz decompression).
	if r.pluginDir != "" {
		readTextFn := func(relPath string) (string, error) {
			return r.readAssetText(relPath)
		}
		assetsBridge := vm.NewObject()
		assetsBridge.Set("readText", readTextFn)
		assetsBridge.Set("readJSON", func(relPath string) (any, error) {
			raw, rErr := r.readAssetBytes(relPath)
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

	// Cross-VM shared native Aho-Corasick wordFilter bridge (zero per-VM trie duplication & zero reflection on hot path).
	wfBridge := vm.NewObject()
	noMatchObj := &jsObject{
		props: map[string]jsValue{
			"matched":  jsFalseVal,
			"word":     {kind: valString},
			"category": {kind: valString},
		},
	}
	wfBridge.setVal("revision", nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
		return jsValue{kind: valNumber, num: float64(r.wordRev.Load())}, nil
	}))
	wfBridge.setVal("bumpRevision", nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
		return jsValue{kind: valNumber, num: float64(r.bumpWordRevision())}, nil
	}))
	wfBridge.setVal("has", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) < 1 {
			return jsFalseVal, nil
		}
		if r.hasWordMatcher(args[0].toString()) {
			return jsTrueVal, nil
		}
		return jsFalseVal, nil
	}))
	wfBridge.Set("stats", func(sig string) map[string]any {
		return r.wordMatcherStats(sig)
	})
	wfBridge.Set("compile", func(sig string, spec map[string]any) (map[string]any, error) {
		return r.compileWordMatcher(wrapper.activeCtx(), sig, spec, wrapper.setNativeBudget)
	})
	wfBridge.setVal("contains", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) < 2 {
			return jsFalseVal, nil
		}
		m := r.lookupWordMatcher(args[0].toString())
		if m != nil && m.Contains(args[1].toString()) {
			return jsTrueVal, nil
		}
		return jsFalseVal, nil
	}))
	wfBridge.setVal("match", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) < 2 {
			return jsNullVal, nil
		}
		m := r.lookupWordMatcher(args[0].toString())
		if m == nil {
			return jsNullVal, nil
		}
		res := m.Match(args[1].toString())
		if !res.Matched {
			if len(noMatchObj.props) != 3 ||
				noMatchObj.props["matched"] != jsFalseVal ||
				noMatchObj.props["word"].kind != valString || noMatchObj.props["word"].str != "" ||
				noMatchObj.props["category"].kind != valString || noMatchObj.props["category"].str != "" {
				noMatchObj = &jsObject{
					props: map[string]jsValue{
						"matched":  jsFalseVal,
						"word":     {kind: valString},
						"category": {kind: valString},
					},
				}
			}
			return jsValue{kind: valObject, obj: noMatchObj}, nil
		}
		return jsValue{
			kind: valObject,
			obj: &jsObject{
				props: map[string]jsValue{
					"matched":  jsTrueVal,
					"word":     {kind: valString, str: res.Word},
					"category": {kind: valString, str: res.Category},
				},
			},
		}, nil
	}))
	wfBridge.setVal("maskText", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) < 2 {
			return jsValue{kind: valString}, nil
		}
		raw := args[1].toString()
		m := r.lookupWordMatcher(args[0].toString())
		if m == nil {
			return jsValue{kind: valString, str: raw}, nil
		}
		return jsValue{kind: valString, str: m.MaskText(raw)}, nil
	}))
	wfBridge.setVal("maskHTML", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
		if len(args) < 2 {
			return jsValue{kind: valString}, nil
		}
		raw := args[1].toString()
		m := r.lookupWordMatcher(args[0].toString())
		if m == nil {
			return jsValue{kind: valString, str: raw}, nil
		}
		return jsValue{kind: valString, str: m.MaskHTML(raw)}, nil
	}))
	wfBridge.Set("setSanitizer", func(spec map[string]any) {
		r.setSanitizerConfig(spec)
	})
	wfBridge.setVal("clear", nativeVal(func(_ *jsRuntime, _ jsValue, _ []jsValue) (jsValue, error) {
		return jsValue{kind: valNumber, num: float64(r.clearWordMatchers())}, nil
	}))
	if r.httpEnabled && r.manifest.CanWriteKV() && r.kv != nil {
		wfBridge.Set("syncRemote", func(opts map[string]any) (map[string]any, error) {
			return r.syncRemoteWordDict(wrapper.activeCtx(), opts, wrapper.setNativeBudget)
		})
	}
	voloObj.Set("wordFilter", wfBridge)

	// Capability 2: Outbound SSRF-safe HTTP client (ONLY injected if allowed_hosts / http.outbound:<host> / http:outbound+url-config declared).
	if r.httpEnabled {
		httpBridge := vm.NewObject()
		httpBridge.Set("fetch", func(rawURL string, opts map[string]any) (map[string]any, error) {
			return r.executeFetch(wrapper.activeCtx(), rawURL, opts)
		})
		voloObj.Set("http", httpBridge)
	}

	// Capability 3: Unified scheduled task registry (`volo.cron`, ONLY injected when "cron" permission is declared).
	if r.manifest.HasPermission("cron") && r.cron != nil {
		cronBridge := vm.NewObject()
		cronBridge.Set("register", func(name string, opts map[string]any) (bool, error) {
			if opts == nil {
				return false, errors.New("volo.cron.register requires an options object")
			}
			handler, _ := opts["handler"].(string)
			intervalSec := parseCronIntOption(opts["interval_seconds"])
			if intervalSec <= 0 {
				intervalSec = parseCronIntOption(opts["intervalSeconds"])
			}
			immediate, _ := opts["immediate"].(bool)
			if err := r.cron.RegisterJob(r.manifest.ID, CronSpec{
				Name:            name,
				IntervalSeconds: intervalSec,
				Handler:         handler,
			}, CronSourceDynamic, immediate); err != nil {
				return false, err
			}
			return true, nil
		})
		cronBridge.Set("unregister", func(name string) bool {
			return r.cron.UnregisterJob(r.manifest.ID, name)
		})
		cronBridge.setVal("trigger", nativeVal(func(_ *jsRuntime, _ jsValue, args []jsValue) (jsValue, error) {
			jobName := ""
			if len(args) >= 1 && args[0].kind != valUndefined && args[0].kind != valNull {
				jobName = args[0].toString()
			}
			cnt := r.cron.Trigger(r.manifest.ID, jobName)
			return jsValue{kind: valNumber, num: float64(cnt)}, nil
		}))
		cronBridge.Set("list", func() []CronJobSnapshot {
			return r.cron.ListJobs(r.manifest.ID)
		})
		voloObj.Set("cron", cronBridge)
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

	// Shallow-freeze host bridge namespaces and builtin prototypes, and snapshot post-init globals
	// so pooled VMs cannot leak prototype or globalThis mutations across invocations.
	for _, pv := range voloObj.props {
		if pv.kind == valObject && pv.obj != nil && pv.obj.scope == nil {
			pv.obj.frozen = true
		} else if pv.kind == valFunc && pv.fn != nil {
			pv.fn.frozenProps = true
		}
	}
	if vm.builtinGlobals != nil {
		vm.builtinGlobals["console"] = jsValue{kind: valObject, obj: consoleObj}
		vm.builtinGlobals["volo"] = jsValue{kind: valObject, obj: voloObj}
		vm.builtinGlobals["ctx"] = jsValue{kind: valObject, obj: voloObj}
	}
	vm.snapshotPostInit()

	modVal := moduleObj.get("exports")
	if modVal.kind == valObject && modVal.obj != nil {
		wrapper.exports = modVal.obj
	} else {
		wrapper.exports = exportsObj
	}
	return wrapper, nil
}

func parseCronIntOption(raw any) int {
	switch v := raw.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	default:
		return 0
	}
}

type multiReadCloser struct {
	reader  io.Reader
	closers []io.Closer
}

func (m *multiReadCloser) Read(p []byte) (int, error) {
	return m.reader.Read(p)
}

func (m *multiReadCloser) Close() error {
	var firstErr error
	for _, c := range m.closers {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *gojaPluginRuntime) openConfinedAssetFile(relPath string) (*os.File, error) {
	if r.pluginDir == "" {
		return nil, errors.New("plugin directory unavailable")
	}
	trimmed := strings.TrimPrefix(strings.TrimSpace(relPath), "./")
	cleanRel, err := sanitizeRelativeAssetPath(trimmed)
	if err != nil {
		return nil, fmt.Errorf("plugin asset %q: %w", relPath, err)
	}
	f, err := os.OpenInRoot(r.pluginDir, filepath.FromSlash(cleanRel))
	if err != nil {
		return nil, fmt.Errorf("plugin asset %q: %w", relPath, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("plugin asset %q: %w", relPath, err)
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("plugin asset %q: not a regular file", relPath)
	}
	if st.Size() > maxPluginAssetReadBytes {
		_ = f.Close()
		return nil, fmt.Errorf("plugin asset %q: exceeds %d MiB limit", relPath, maxPluginAssetReadBytes>>20)
	}
	return f, nil
}

func (r *gojaPluginRuntime) resolvePluginAssetPath(relPath string) (string, error) {
	f, err := r.openConfinedAssetFile(relPath)
	if err != nil {
		return "", err
	}
	_ = f.Close()
	trimmed := strings.TrimPrefix(strings.TrimSpace(relPath), "./")
	cleanRel, _ := sanitizeRelativeAssetPath(trimmed)
	return filepath.Join(r.pluginDir, filepath.FromSlash(cleanRel)), nil
}

func (r *gojaPluginRuntime) openConfinedAssetReader(relPath string) (io.ReadCloser, error) {
	f, err := r.openConfinedAssetFile(relPath)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(strings.ToLower(strings.TrimSpace(relPath)), ".gz") {
		zr, zErr := gzip.NewReader(f)
		if zErr != nil {
			_ = f.Close()
			return nil, fmt.Errorf("plugin asset %q: invalid gzip stream: %w", relPath, zErr)
		}
		return &multiReadCloser{
			reader:  io.LimitReader(zr, maxPluginAssetReadBytes+1),
			closers: []io.Closer{zr, f},
		}, nil
	}
	return &multiReadCloser{
		reader:  io.LimitReader(f, maxPluginAssetReadBytes+1),
		closers: []io.Closer{f},
	}, nil
}

func (r *gojaPluginRuntime) readAssetBytes(relPath string) ([]byte, error) {
	rc, err := r.openConfinedAssetReader(relPath)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	if len(data) > maxPluginAssetReadBytes {
		return nil, fmt.Errorf("plugin asset %q: exceeds %d MiB decompressed limit", relPath, maxPluginAssetReadBytes>>20)
	}
	return data, nil
}

func (r *gojaPluginRuntime) readAssetText(relPath string) (string, error) {
	data, err := r.readAssetBytes(relPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (r *gojaPluginRuntime) hasWordMatcher(sig string) bool {
	key := strings.TrimSpace(sig)
	if key == "" {
		return false
	}
	r.wordMu.RLock()
	_, ok := r.wordMatchers[key]
	r.wordMu.RUnlock()
	return ok
}

func (r *gojaPluginRuntime) wordMatcherStats(sig string) map[string]any {
	key := strings.TrimSpace(sig)
	r.wordMu.RLock()
	m := r.wordMatchers[key]
	r.wordMu.RUnlock()
	if m == nil {
		return kvMap("compiled", false, "word_count", 0, "allow_count", 0)
	}
	return kvMap("compiled", true, "word_count", m.WordCount(), "allow_count", m.AllowCount())
}

func addWordsToBuilderFromAny(bld *wordfilter.Builder, raw any, category string) {
	switch v := raw.(type) {
	case string:
		bld.AddWordLines(v, category)
	case []string:
		for _, s := range v {
			bld.AddWordLines(s, category)
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				bld.AddWordLines(s, category)
			}
		}
	}
}

func addAllowToBuilderFromAny(bld *wordfilter.Builder, raw any) {
	switch v := raw.(type) {
	case string:
		d, a := wordfilter.ParseWordLines(v)
		for _, w := range d {
			bld.AddAllow(w)
		}
		for _, w := range a {
			bld.AddAllow(w)
		}
	case []string:
		for _, s := range v {
			addAllowToBuilderFromAny(bld, s)
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				addAllowToBuilderFromAny(bld, s)
			}
		}
	}
}

type remoteDictMeta struct {
	URL          string `json:"url"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	SyncedAt     string `json:"synced_at"`
	CheckedAt    string `json:"checked_at"`
	WordCount    int    `json:"word_count"`
	AllowCount   int    `json:"allow_count"`
	TaggedCount  int    `json:"tagged_count"`
	DroppedCount int    `json:"dropped_count"`
	Chunks       int    `json:"chunks"`
	Encoding     string `json:"encoding"`
	Mode         string `json:"mode"`
}

func (r *gojaPluginRuntime) loadRemoteGzipReader(ctx context.Context, kvPrefix string) (*remoteDictMeta, io.ReadCloser, error) {
	if !r.manifest.CanReadKV() || r.kv == nil || kvPrefix == "" {
		return nil, nil, nil
	}
	rawMeta, ok, err := r.kv.Get(ctx, kvPrefix+":meta")
	if err != nil || !ok || strings.TrimSpace(rawMeta) == "" {
		return nil, nil, err
	}
	var meta remoteDictMeta
	if json.Unmarshal([]byte(rawMeta), &meta) != nil || meta.Chunks <= 0 {
		return nil, nil, nil
	}
	if meta.Chunks > 128 {
		meta.Chunks = 128
	}

	if meta.Encoding == "gzip+base64" {
		var gzBuf bytes.Buffer
		gzBuf.Grow(meta.Chunks * (remoteDictBase64ChunkLen * 3 / 4))
		for i := 0; i < meta.Chunks; i++ {
			chunkStr, cOk, cErr := r.kv.Get(ctx, fmt.Sprintf("%s:gz:%d", kvPrefix, i))
			if cErr != nil || !cOk || chunkStr == "" {
				return &meta, nil, fmt.Errorf("missing remote chunk %d", i)
			}
			decoded, dErr := base64.StdEncoding.DecodeString(chunkStr)
			if dErr != nil {
				return &meta, nil, dErr
			}
			gzBuf.Write(decoded)
			if gzBuf.Len() > maxPluginAssetReadBytes {
				return &meta, nil, errors.New("remote compressed dictionary exceeds size limit")
			}
		}
		zr, zErr := gzip.NewReader(bytes.NewReader(gzBuf.Bytes()))
		if zErr != nil {
			return &meta, nil, zErr
		}
		return &meta, &multiReadCloser{
			reader:  io.LimitReader(zr, maxPluginAssetReadBytes+1),
			closers: []io.Closer{zr},
		}, nil
	}

	// Legacy plain-text chunk fallback ("remote:chunk:<i/>").
	var plainBuf bytes.Buffer
	for i := 0; i < meta.Chunks; i++ {
		chunkStr, cOk, cErr := r.kv.Get(ctx, fmt.Sprintf("%s:chunk:%d", kvPrefix, i))
		if cErr == nil && cOk && chunkStr != "" {
			if plainBuf.Len() > 0 {
				plainBuf.WriteByte('\n')
			}
			plainBuf.WriteString(chunkStr)
		}
	}
	return &meta, io.NopCloser(bytes.NewReader(plainBuf.Bytes())), nil
}

func (r *gojaPluginRuntime) compileWordMatcher(ctx context.Context, sig string, spec map[string]any, setBudget func(time.Duration)) (map[string]any, error) {
	key := strings.TrimSpace(sig)
	if key == "" || len(key) > 256 {
		return nil, errors.New("volo.wordFilter.compile: signature must be 1..256 characters")
	}

	if setBudget != nil {
		setBudget(3 * time.Second)
		defer setBudget(0)
	}

	r.wordCompileMu.Lock()
	defer r.wordCompileMu.Unlock()

	r.wordMu.RLock()
	existing := r.wordMatchers[key]
	r.wordMu.RUnlock()
	if existing != nil {
		return kvMap("word_count", existing.WordCount(), "allow_count", existing.AllowCount()), nil
	}

	var bOpts wordfilter.BuilderOptions
	if spec != nil {
		if ig, ok := spec["ignore_separators"].(bool); ok {
			bOpts.IgnoreSeparators = ig
		}
		if ir, ok := spec["ignore_repeat"].(bool); ok {
			bOpts.IgnoreRepeat = ir
		}
	}

	bld := wordfilter.NewBuilder(bOpts)

	if spec != nil {
		var enabledCats map[string]bool
		if rawCats, ok := spec["enabled_categories"].(map[string]any); ok && len(rawCats) > 0 {
			enabledCats = make(map[string]bool, len(rawCats))
			for k, v := range rawCats {
				if bv, bOk := v.(bool); bOk {
					enabledCats[strings.TrimSpace(k)] = bv
				}
			}
		}

		houbbAsset, _ := spec["houbb_asset"].(string)
		houbbAsset = strings.TrimSpace(houbbAsset)
		if houbbAsset == "" {
			baseAlias, _ := spec["base_asset"].(string)
			houbbAsset = strings.TrimSpace(baseAlias)
		}
		remotePrefix, _ := spec["remote_kv_prefix"].(string)
		remotePrefix = strings.TrimSpace(remotePrefix)
		remoteMode, _ := spec["remote_mode"].(string)
		remoteMode = strings.ToLower(strings.TrimSpace(remoteMode))
		if remoteMode == "" {
			remoteMode = "auto"
		}

		if houbbAsset != "" {
			bld.Reserve(65536, 128)
		}

		streamedRemoteReplace := false
		if remotePrefix != "" {
			meta, remRC, remErr := r.loadRemoteGzipReader(ctx, remotePrefix)
			if remErr == nil && remRC != nil {
				if meta != nil && meta.WordCount > 0 {
					bld.Reserve(meta.WordCount, max(meta.AllowCount, 64))
				}
				added, _, sErr := bld.AddHoubbStream(remRC, enabledCats)
				_ = remRC.Close()
				if sErr == nil && added > 0 {
					isReplace := remoteMode == "replace" || (remoteMode == "auto" && meta != nil && meta.Mode == "replace")
					if isReplace {
						streamedRemoteReplace = true
					}
				}
			}
		}

		if houbbAsset != "" && !streamedRemoteReplace {
			rc, aErr := r.openConfinedAssetReader(houbbAsset)
			if aErr != nil {
				return nil, fmt.Errorf("volo.wordFilter.compile houbb_asset: %w", aErr)
			}
			_, _, sErr := bld.AddHoubbStream(rc, enabledCats)
			_ = rc.Close()
			if sErr != nil {
				return nil, fmt.Errorf("volo.wordFilter.compile houbb_asset stream: %w", sErr)
			}
		}

		rawAllowAssets := spec["allow_assets"]
		if rawAllowAssets == nil {
			rawAllowAssets = spec["allow_asset"]
		}
		if rawAllowAssets != nil {
			var paths []string
			switch av := rawAllowAssets.(type) {
			case string:
				paths = []string{av}
			case []any:
				for _, it := range av {
					if s, sOk := it.(string); sOk {
						paths = append(paths, s)
					}
				}
			}
			for _, rel := range paths {
				rel = strings.TrimSpace(rel)
				if rel == "" {
					continue
				}
				if txt, rErr := r.readAssetText(rel); rErr == nil && txt != "" {
					addAllowToBuilderFromAny(bld, txt)
				}
			}
		}

		if rawGroups, ok := spec["groups"].([]any); ok {
			for _, gItem := range rawGroups {
				gm, gOk := gItem.(map[string]any)
				if !gOk {
					continue
				}
				cat, _ := gm["category"].(string)
				if txt, tOk := gm["text"].(string); tOk && txt != "" {
					bld.AddWordLines(txt, cat)
				}
				if rawW, hasW := gm["words"]; hasW && rawW != nil {
					addWordsToBuilderFromAny(bld, rawW, cat)
				}
			}
		}
		if rawDeny, hasD := spec["deny"]; hasD && rawDeny != nil {
			addWordsToBuilderFromAny(bld, rawDeny, "custom")
		}
		if rawAllow, hasA := spec["allow"]; hasA && rawAllow != nil {
			addAllowToBuilderFromAny(bld, rawAllow)
		}
	}

	compiled := bld.Build()
	nsPrefix := wordSigNamespace(key)

	r.wordMu.Lock()
	if _, exists := r.wordMatchers[key]; !exists {
		if nsPrefix != "" && len(r.wordOrder) > 0 {
			kept := r.wordOrder[:0]
			for _, oldKey := range r.wordOrder {
				if strings.HasPrefix(oldKey, nsPrefix) {
					delete(r.wordMatchers, oldKey)
					continue
				}
				kept = append(kept, oldKey)
			}
			r.wordOrder = kept
		}
		for len(r.wordOrder) >= maxCachedWordMatcherSigs {
			oldest := r.wordOrder[0]
			r.wordOrder = r.wordOrder[1:]
			delete(r.wordMatchers, oldest)
		}
		r.wordOrder = append(r.wordOrder, key)
	}
	r.wordMatchers[key] = compiled
	r.wordMu.Unlock()

	return kvMap("word_count", compiled.WordCount(), "allow_count", compiled.AllowCount()), nil
}

func wordSigNamespace(sig string) string {
	if colon := strings.IndexByte(sig, ':'); colon > 0 && colon <= 16 {
		return sig[:colon+1]
	}
	return ""
}

// syncRemoteWordDict fetches an upstream dictionary (such as houbb/sensitive-word-data's
// sensitive_word_dict.txt), streams & sanitizes/deduplicates/tag-enriches it natively in Go without
// allocating huge JS strings, persists compressed base64 chunks in plugin_kv, and bumps wordRev.
func (r *gojaPluginRuntime) syncRemoteWordDict(ctx context.Context, opts map[string]any, setBudget func(time.Duration)) (map[string]any, error) {
	if !r.httpEnabled || !r.manifest.CanWriteKV() || r.kv == nil {
		return nil, errors.New("volo.wordFilter.syncRemote requires outbound HTTP and KV write permissions")
	}
	if setBudget != nil {
		setBudget(12 * time.Second)
		defer setBudget(0)
	}
	rawURL, _ := opts["url"].(string)
	rawURL = strings.TrimSpace(rawURL)
	kvPrefix, _ := opts["kv_prefix"].(string)
	kvPrefix = strings.TrimSpace(kvPrefix)
	if kvPrefix == "" {
		kvPrefix = "remote"
	}
	nowISO := time.Now().UTC().Format(time.RFC3339)
	failSync := func(errMsg string, persist bool) (map[string]any, error) {
		if persist {
			_ = r.kv.Set(ctx, kvPrefix+":last_error", nowISO+" "+errMsg, 0)
		}
		return kvMap("ok", false, "error", errMsg), nil
	}
	if rawURL == "" {
		return failSync("update_url is empty", false)
	}
	force, _ := opts["force"].(bool)
	baseTagAsset, _ := opts["base_tag_asset"].(string)
	baseTagAsset = strings.TrimSpace(baseTagAsset)
	replaceThreshold := parseCronIntOption(opts["replace_threshold"])
	if replaceThreshold <= 0 {
		replaceThreshold = 10_000
	}

	httpClient, err := r.getHTTPClient()
	if err != nil {
		return failSync(err.Error(), false)
	}

	var prevMeta remoteDictMeta
	hasPrevMeta := false
	if rawPrev, ok, _ := r.kv.Get(ctx, kvPrefix+":meta"); ok && rawPrev != "" {
		hasPrevMeta = json.Unmarshal([]byte(rawPrev), &prevMeta) == nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return failSync(err.Error(), false)
	}
	req.Header.Set("User-Agent", "Volo-go-Plugin/"+r.manifest.ID)
	req.Header.Set("Accept", "text/plain, application/json;q=0.9")
	if !force && hasPrevMeta && prevMeta.URL == rawURL && prevMeta.Chunks > 0 {
		if prevMeta.ETag != "" {
			req.Header.Set("If-None-Match", prevMeta.ETag)
		}
		if prevMeta.LastModified != "" {
			req.Header.Set("If-Modified-Since", prevMeta.LastModified)
		}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return failSync(err.Error(), true)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified && hasPrevMeta {
		prevMeta.CheckedAt = nowISO
		if metaJSON, mErr := json.Marshal(prevMeta); mErr == nil {
			_ = r.kv.Set(ctx, kvPrefix+":meta", string(metaJSON), 0)
		}
		_ = r.kv.Delete(ctx, kvPrefix+":last_error")
		return kvMap("ok", true, "not_modified", true, "line_count", prevMeta.WordCount, "word_count", prevMeta.WordCount, "allow_count", prevMeta.AllowCount, "tagged_count", prevMeta.TaggedCount, "dropped_count", prevMeta.DroppedCount, "chunks", prevMeta.Chunks, "mode", prevMeta.Mode, "synced_at", prevMeta.SyncedAt), nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return failSync(fmt.Sprintf("HTTP %d", resp.StatusCode), true)
	}
	if resp.ContentLength > maxPluginFetchBodyBytes {
		return failSync(fmt.Sprintf("remote payload exceeds %d MiB limit", maxPluginFetchBodyBytes>>20), true)
	}

	var baseTags map[string]string
	if baseTagAsset != "" {
		if tagRC, tErr := r.openConfinedAssetReader(baseTagAsset); tErr == nil {
			baseTags, _ = wordfilter.LoadHoubbTagMap(tagRC)
			_ = tagRC.Close()
		}
	}

	convStats, gzBytes, convErr := wordfilter.ConvertHoubbStream(resp.Body, maxPluginFetchBodyBytes, baseTags)
	baseTags = nil // Release tag lookup map immediately before KV writes
	if convErr != nil {
		if errors.Is(convErr, wordfilter.ErrHoubbPayloadTooLarge) {
			return failSync(fmt.Sprintf("remote payload exceeds %d MiB limit", maxPluginFetchBodyBytes>>20), true)
		}
		return failSync(convErr.Error(), true)
	}

	var chunkCount int
	if len(gzBytes) > 0 {
		encBuf := make([]byte, remoteDictBase64ChunkLen)
		for rawOff := 0; rawOff < len(gzBytes); rawOff += remoteDictRawChunkBytes {
			rawEnd := min(rawOff+remoteDictRawChunkBytes, len(gzBytes))
			encLen := base64.StdEncoding.EncodedLen(rawEnd - rawOff)
			base64.StdEncoding.Encode(encBuf[:encLen], gzBytes[rawOff:rawEnd])
			if sErr := r.kv.Set(ctx, fmt.Sprintf("%s:gz:%d", kvPrefix, chunkCount), string(encBuf[:encLen]), 0); sErr != nil {
				return failSync(sErr.Error(), true)
			}
			chunkCount++
		}
	}
	if hasPrevMeta {
		for stale := chunkCount; stale < prevMeta.Chunks; stale++ {
			_ = r.kv.Delete(ctx, fmt.Sprintf("%s:gz:%d", kvPrefix, stale))
			_ = r.kv.Delete(ctx, fmt.Sprintf("%s:chunk:%d", kvPrefix, stale))
		}
	}

	mode := "merge"
	if convStats.WordCount >= replaceThreshold {
		mode = "replace"
	}
	nextMeta := remoteDictMeta{
		URL:          rawURL,
		ETag:         strings.TrimSpace(resp.Header.Get("ETag")),
		LastModified: strings.TrimSpace(resp.Header.Get("Last-Modified")),
		SyncedAt:     nowISO,
		CheckedAt:    nowISO,
		WordCount:    convStats.WordCount,
		AllowCount:   convStats.AllowCount,
		TaggedCount:  convStats.TaggedCount,
		DroppedCount: convStats.DroppedCount,
		Chunks:       chunkCount,
		Encoding:     "gzip+base64",
		Mode:         mode,
	}
	metaBytes, _ := json.Marshal(nextMeta)
	if sErr := r.kv.Set(ctx, kvPrefix+":meta", string(metaBytes), 0); sErr != nil {
		return failSync(sErr.Error(), false)
	}
	_ = r.kv.Delete(ctx, kvPrefix+":last_error")

	// Drop previous compiled matchers immediately so old 3.2 MiB state arrays are eligible for GC
	// before the updated matcher is compiled on next check.
	r.clearWordMatchers()

	return kvMap("ok", true, "not_modified", false, "line_count", convStats.WordCount, "word_count", convStats.WordCount, "allow_count", convStats.AllowCount, "tagged_count", convStats.TaggedCount, "dropped_count", convStats.DroppedCount, "gzip_bytes", convStats.GzipBytes, "chunks", chunkCount, "mode", mode, "synced_at", nowISO), nil
}

func (r *gojaPluginRuntime) lookupWordMatcher(sig string) *wordfilter.Matcher {
	key := strings.TrimSpace(sig)
	if key == "" {
		return nil
	}
	r.wordMu.RLock()
	m := r.wordMatchers[key]
	r.wordMu.RUnlock()
	return m
}

func (r *gojaPluginRuntime) clearWordMatchers() int64 {
	r.wordMu.Lock()
	clear(r.wordMatchers)
	r.wordOrder = r.wordOrder[:0]
	r.sanitizerCfg = runtimeSanitizerConfig{}
	nextRev := r.wordRev.Add(1)
	r.wordMu.Unlock()
	return nextRev
}

func (r *gojaPluginRuntime) bumpWordRevision() int64 {
	r.wordMu.Lock()
	r.sanitizerCfg.initialized = false
	nextRev := r.wordRev.Add(1)
	r.wordMu.Unlock()
	return nextRev
}

func (r *gojaPluginRuntime) setSanitizerConfig(spec map[string]any) {
	if r == nil {
		return
	}
	var cfg runtimeSanitizerConfig
	cfg.initialized = true
	if spec != nil {
		cfg.enabled, _ = spec["enabled"].(bool)
		if cs, ok := spec["content_sig"].(string); ok {
			cfg.contentSig = strings.TrimSpace(cs)
		}
		if ns, ok := spec["nickname_sig"].(string); ok {
			cfg.nicknameSig = strings.TrimSpace(ns)
		}
		cfg.checkNickname, _ = spec["check_nickname"].(bool)
	}
	r.wordMu.Lock()
	cfg.forWordRev = r.wordRev.Load()
	r.sanitizerCfg = cfg
	r.wordMu.Unlock()
}

func (r *gojaPluginRuntime) readReadySanitizerMatchers(needNickname bool) (*wordfilter.Matcher, *wordfilter.Matcher, bool) {
	curRev := r.wordRev.Load()
	r.wordMu.RLock()
	sc := r.sanitizerCfg
	if !sc.initialized || sc.forWordRev != curRev {
		r.wordMu.RUnlock()
		return nil, nil, false
	}
	if !sc.enabled {
		r.wordMu.RUnlock()
		return nil, nil, true
	}
	var contentMatcher, nickMatcher *wordfilter.Matcher
	if sc.contentSig != "" {
		contentMatcher = r.wordMatchers[sc.contentSig]
		if contentMatcher == nil {
			r.wordMu.RUnlock()
			return nil, nil, false
		}
	}
	if needNickname && sc.checkNickname && sc.nicknameSig != "" {
		nickMatcher = r.wordMatchers[sc.nicknameSig]
		if nickMatcher == nil {
			r.wordMu.RUnlock()
			return nil, nil, false
		}
	}
	r.wordMu.RUnlock()
	return contentMatcher, nickMatcher, true
}

func (r *gojaPluginRuntime) ensureSanitizerMatchers(ctx context.Context, needNickname bool) (*wordfilter.Matcher, *wordfilter.Matcher) {
	if r == nil {
		return nil, nil
	}
	if cm, nm, ok := r.readReadySanitizerMatchers(needNickname); ok {
		return cm, nm
	}

	r.sanitizerWarmMu.Lock()
	defer r.sanitizerWarmMu.Unlock()

	if cm, nm, ok := r.readReadySanitizerMatchers(needNickname); ok {
		return cm, nm
	}

	if !r.hasCallable("ensureSanitizer") {
		r.wordMu.Lock()
		r.sanitizerCfg = runtimeSanitizerConfig{
			initialized: true,
			forWordRev:  r.wordRev.Load(),
			enabled:     false,
		}
		r.wordMu.Unlock()
		return nil, nil
	}

	warmCtx := context.Background()
	if ctx != nil {
		warmCtx = context.WithoutCancel(ctx)
	}
	if _, _, err := r.invoke(warmCtx, 4*time.Second, "ensureSanitizer"); err != nil {
		if r.logger != nil {
			r.logger.Write(r.manifest.ID, "warn", "sanitizer", "lazy sanitizer warmup failed: "+err.Error())
		}
		return nil, nil
	}
	cm, nm, _ := r.readReadySanitizerMatchers(needNickname)
	return cm, nm
}

func (r *gojaPluginRuntime) maskSanitizerHTML(ctx context.Context, htmlStr string) string {
	if r == nil || strings.TrimSpace(htmlStr) == "" {
		return htmlStr
	}
	cm, _ := r.ensureSanitizerMatchers(ctx, false)
	if cm == nil {
		return htmlStr
	}
	return cm.MaskHTML(htmlStr)
}

func (r *gojaPluginRuntime) maskSanitizerText(ctx context.Context, text string) string {
	if r == nil || strings.TrimSpace(text) == "" {
		return text
	}
	cm, _ := r.ensureSanitizerMatchers(ctx, false)
	if cm == nil {
		return text
	}
	return cm.MaskText(text)
}

func (r *gojaPluginRuntime) maskSanitizerNickname(ctx context.Context, nickname string, isStaffOrAuthor bool) string {
	if r == nil || isStaffOrAuthor || strings.TrimSpace(nickname) == "" {
		return nickname
	}
	cm, nm := r.ensureSanitizerMatchers(ctx, true)
	out := nickname
	if cm != nil {
		out = cm.MaskText(out)
	}
	if nm != nil {
		out = nm.MaskText(out)
	}
	return out
}

func (r *gojaPluginRuntime) resetHTTPClient() {
	if r == nil {
		return
	}
	r.httpMu.Lock()
	oldClient := r.httpClientPtr
	r.httpClientPtr = nil
	r.httpHostsKey = ""
	r.httpMu.Unlock()
	if oldClient != nil {
		oldClient.CloseIdleConnections()
	}
}

func (r *gojaPluginRuntime) resetPool() {
	if r == nil {
		return
	}
	for {
		select {
		case <-r.pool:
		default:
			return
		}
	}
}

func (r *gojaPluginRuntime) closeRuntime() {
	if r == nil {
		return
	}
	r.clearWordMatchers()
	r.resetHTTPClient()
	r.resetPool()
}

func (p *pooledVM) activeCtx() context.Context {
	if p.reqCtx != nil {
		return p.reqCtx
	}
	return context.Background()
}

func computeEffectiveOutboundHosts(
	manifest Manifest,
	cfg map[string]any,
	resolveVal func(any) any,
	allowLoopbackForTests bool,
) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(h string) {
		clean := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
		if clean != "" && !seen[clean] {
			seen[clean] = true
			out = append(out, clean)
		}
	}
	for _, h := range manifest.EffectiveAllowedHosts() {
		add(h)
	}
	if (manifest.HasPermission("http:outbound") || manifest.HasPermission("net:http") || len(out) > 0) && len(cfg) > 0 {
		for _, f := range manifest.ConfigSchema {
			if f.Type != "url" {
				continue
			}
			v := cfg[f.Key]
			if resolveVal != nil {
				v = resolveVal(v)
			}
			rawVal, _ := v.(string)
			rawVal = strings.TrimSpace(rawVal)
			if rawVal == "" || strings.HasPrefix(rawVal, "/") {
				continue
			}
			// Never auto-authorize an unvetted manifest.json default URL unless its host is already
			// explicitly declared in manifest.allowed_hosts; dynamic host expansion requires an
			// explicit administrator-configured URL override!
			if defStr, isDefStr := f.Default.(string); isDefStr && rawVal == strings.TrimSpace(defStr) {
				continue
			}
			u, err := url.Parse(rawVal)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				continue
			}
			host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(u.Hostname()), "."))
			if host == "" {
				continue
			}
			if _, vErr := validateAllowedHostPattern(host); vErr == nil {
				add(host)
			} else if allowLoopbackForTests && (host == "127.0.0.1" || host == "::1" || host == "localhost") {
				add(host)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (r *gojaPluginRuntime) effectiveOutboundHosts() []string {
	var cfg map[string]any
	if r.getConfig != nil {
		cfg = r.getConfig()
	}
	return computeEffectiveOutboundHosts(r.manifest, cfg, r.resolveRuntimeConfigValue, r.allowLoopbackForTests)
}

func (r *gojaPluginRuntime) getHTTPClient() (*http.Client, error) {
	allowed := r.effectiveOutboundHosts()
	if len(allowed) == 0 {
		return nil, fmt.Errorf("%w: no allowed outbound hosts configured", security.ErrSSRFBlocked)
	}
	key := strings.Join(allowed, ",")

	r.httpMu.Lock()
	if r.httpClientPtr != nil && r.httpHostsKey == key {
		client := r.httpClientPtr
		r.httpMu.Unlock()
		return client, nil
	}
	oldClient := r.httpClientPtr
	dialer := &security.OutboundSafeDialer{
		AllowedHosts:          allowed,
		Timeout:               4 * time.Second,
		Resolver:              r.customResolver,
		AllowLoopbackForTests: r.allowLoopbackForTests,
	}
	client := dialer.NewHTTPClient(10 * time.Second)
	r.httpHostsKey = key
	r.httpClientPtr = client
	r.httpMu.Unlock()
	if oldClient != nil {
		oldClient.CloseIdleConnections()
	}
	return client, nil
}

func decodeFetchBase64(s string, maxBytes int) ([]byte, error) {
	var clean strings.Builder
	clean.Grow(len(s))
	maxEncoded := base64.StdEncoding.EncodedLen(maxBytes) + 4
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r', '\f':
		default:
			clean.WriteByte(s[i])
			if clean.Len() > maxEncoded {
				return nil, fmt.Errorf("plugin http.fetch: body_base64 exceeds %d MiB limit", maxBytes>>20)
			}
		}
	}
	enc := clean.String()
	decoded, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(enc, "="))
		if err != nil {
			return nil, fmt.Errorf("plugin http.fetch: invalid body_base64: %w", err)
		}
	}
	if len(decoded) > maxBytes {
		return nil, fmt.Errorf("plugin http.fetch: body_base64 exceeds %d MiB limit", maxBytes>>20)
	}
	return decoded, nil
}

func (r *gojaPluginRuntime) executeFetch(ctx context.Context, rawURL string, opts map[string]any) (map[string]any, error) {
	httpClient, err := r.getHTTPClient()
	if err != nil {
		return nil, err
	}
	method := http.MethodGet
	var bodyReader io.Reader
	var contentLen int64 = -1
	headers := make(map[string]string)
	wantBase64Resp := false

	if opts != nil {
		if m, ok := opts["method"].(string); ok && strings.TrimSpace(m) != "" {
			method = strings.ToUpper(strings.TrimSpace(m))
		}
		rawB64, hasB64 := opts["body_base64"].(string)
		if !hasB64 {
			rawB64, hasB64 = opts["bodyBase64"].(string)
		}
		if hasB64 {
			if rawB64 != "" {
				decoded, dErr := decodeFetchBase64(rawB64, maxPluginBinaryBodyBytes)
				if dErr != nil {
					return nil, dErr
				}
				bodyReader = bytes.NewReader(decoded)
				contentLen = int64(len(decoded))
			} else {
				bodyReader = bytes.NewReader(nil)
				contentLen = 0
			}
		} else if b, ok := opts["body"].(string); ok && b != "" {
			if len(b) > (1 << 20) {
				return nil, errors.New("plugin http.fetch: request body exceeds 1 MiB")
			}
			bodyReader = strings.NewReader(b)
			contentLen = int64(len(b))
		}
		if enc, ok := opts["response_encoding"].(string); ok && strings.EqualFold(strings.TrimSpace(enc), "base64") {
			wantBase64Resp = true
		} else if enc, ok := opts["responseEncoding"].(string); ok && strings.EqualFold(strings.TrimSpace(enc), "base64") {
			wantBase64Resp = true
		} else if bin, ok := opts["binary"].(bool); ok && bin {
			wantBase64Resp = true
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
	if contentLen >= 0 {
		req.ContentLength = contentLen
	}
	req.Header.Set("User-Agent", "Volo-go-Plugin/"+r.manifest.ID)
	for k, v := range headers {
		if strings.EqualFold(k, "Host") {
			continue
		}
		req.Header.Set(k, v)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respHeaders := make(map[string]string, len(resp.Header))
	for k, vs := range resp.Header {
		if len(vs) > 0 {
			respHeaders[strings.ToLower(k)] = vs[0]
		}
	}

	if wantBase64Resp {
		if resp.ContentLength > maxPluginBinaryBodyBytes {
			return nil, fmt.Errorf("plugin http.fetch: response body exceeds %d MiB limit", maxPluginBinaryBodyBytes>>20)
		}
		bodyBytes, rErr := io.ReadAll(io.LimitReader(resp.Body, maxPluginBinaryBodyBytes+1))
		if rErr != nil {
			return nil, rErr
		}
		if len(bodyBytes) > maxPluginBinaryBodyBytes {
			return nil, fmt.Errorf("plugin http.fetch: response body exceeds %d MiB limit", maxPluginBinaryBodyBytes>>20)
		}
		encoded := base64.StdEncoding.EncodeToString(bodyBytes)
		return kvMap(
			"status", resp.StatusCode,
			"ok", resp.StatusCode >= 200 && resp.StatusCode < 300,
			"headers", respHeaders,
			"body", encoded,
			"body_base64", encoded,
			"byte_length", len(bodyBytes),
		), nil
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxPluginFetchBodyBytes))
	if err != nil {
		return nil, err
	}
	bodyText := string(bodyBytes)
	return kvMap("status", resp.StatusCode, "ok", resp.StatusCode >= 200 && resp.StatusCode < 300, "headers", respHeaders, "body", bodyText), nil
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
			pvm.vm.restorePostInit()
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
		pvm.setNativeBudget = nil
		if !tainted && err == nil && pvm.uses < maxPooledVMUses {
			pvm.vm.restorePostInit()
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
	pvm.setNativeBudget = func(d time.Duration) {
		intMu.Lock()
		if !finished {
			next := timeout
			if d > next {
				next = d
			}
			timer.Reset(next)
		}
		intMu.Unlock()
	}
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
		interceptable := fnName == "onHook" && IsInterceptableHook(hookName)
		hookStage := fnName
		if hookName != "" {
			hookStage = fnName + ":" + hookName
		}

		if ok {
			jsArgs := make([]jsValue, len(args))
			for i, a := range args {
				jsArgs[i] = vm.ToValue(a)
			}
			res, callErr := vm.callFunc(callable, jsUndefinedVal, jsArgs)
			if callErr != nil {
				normErr := normalizeJSError(r.manifest.ID, hookStage, callErr)
				var rej *HookRejectedError
				if interceptable && errors.As(normErr, &rej) {
					return nil, true, false, rej
				}
				return nil, true, true, normErr
			}
			if res.kind != valUndefined && res.kind != valNull {
				if interceptable && isJSHookRejection(res) {
					return res.Export(), true, false, nil
				}
				if fnName == "onHook" {
					curPayloadVal = mergeJSHookPayload(vm, curPayloadVal, res)
					lastRes = curPayloadVal
				} else {
					lastRes = res
				}
			}
		}

		for _, handlerFn := range extraHookHandlers {
			hArgs := []jsValue{curPayloadVal, {kind: valString, str: hookName}}
			res, callErr := vm.callFunc(handlerFn, jsUndefinedVal, hArgs)
			if callErr != nil {
				normErr := normalizeJSError(r.manifest.ID, hookStage, callErr)
				var rej *HookRejectedError
				if interceptable && errors.As(normErr, &rej) {
					return nil, true, false, rej
				}
				return nil, true, true, normErr
			}
			if res.kind != valUndefined && res.kind != valNull {
				if interceptable && isJSHookRejection(res) {
					return res.Export(), true, false, nil
				}
				curPayloadVal = mergeJSHookPayload(vm, curPayloadVal, res)
				lastRes = curPayloadVal
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

func isJSHookRejection(val jsValue) bool {
	if val.kind == valBool && !val.b {
		return true
	}
	if val.kind != valObject || val.obj == nil || val.obj.isError || val.obj.isDate {
		return false
	}
	if v := val.obj.get("__volo_rejected__"); v.kind == valBool && v.b {
		return true
	}
	if v := val.obj.get("reject"); v.kind == valBool && v.b {
		return true
	}
	if v := val.obj.get("allow"); v.kind == valBool && !v.b {
		return true
	}
	if v := val.obj.get("action"); v.kind == valString && strings.EqualFold(strings.TrimSpace(v.str), "reject") {
		return true
	}
	return false
}

func mergeJSHookPayload(vm *jsRuntime, cur jsValue, next jsValue) jsValue {
	if next.kind == valBool && next.b {
		return cur
	}
	if cur.kind != valObject || cur.obj == nil || cur.obj.isError || cur.obj.isDate ||
		next.kind != valObject || next.obj == nil || next.obj.isError || next.obj.isDate {
		return next
	}
	merged := vm.NewObject()
	curPending := false
	for k, v := range cur.obj.props {
		merged.setVal(k, v)
		if k == "status" && v.kind == valString && strings.EqualFold(strings.TrimSpace(v.str), "pending") {
			curPending = true
		}
	}
	if act := next.obj.get("action"); act.kind == valString && strings.EqualFold(strings.TrimSpace(act.str), "moderate") {
		curPending = true
	}
	if mod := next.obj.get("moderate"); mod.kind == valBool && mod.b {
		curPending = true
	}
	for k, v := range next.obj.props {
		switch k {
		case "allow", "reject", "action", "moderate", "reason", "code", "__volo_rejected__":
			continue
		case "status":
			if curPending {
				merged.setVal("status", jsValue{kind: valString, str: "pending"})
				continue
			}
		}
		merged.setVal(k, v)
	}
	if curPending {
		merged.setVal("status", jsValue{kind: valString, str: "pending"})
	}
	return jsValue{kind: valObject, obj: merged}
}

func normalizeJSError(pluginID string, stage string, err error) error {
	var thrown *jsThrownError
	if errors.As(err, &thrown) && strings.HasPrefix(stage, "onHook:") {
		hookName := strings.TrimPrefix(stage, "onHook:")
		if IsInterceptableHook(hookName) && isJSHookRejection(thrown.val) {
			reason := ""
			code := ""
			if thrown.val.kind == valObject && thrown.val.obj != nil {
				if rVal := thrown.val.obj.get("reason"); rVal.kind != valUndefined && rVal.kind != valNull {
					reason = strings.TrimSpace(rVal.toString())
				}
				if cVal := thrown.val.obj.get("code"); cVal.kind != valUndefined && cVal.kind != valNull {
					code = strings.TrimSpace(cVal.toString())
				}
			}
			return &HookRejectedError{
				PluginID: pluginID,
				Hook:     hookName,
				Reason:   reason,
				Code:     code,
			}
		}
	}
	var interrupted *jsInterruptedError
	if errors.As(err, &interrupted) {
		if errors.Is(interrupted, ErrExecutionTimeout) {
			return fmt.Errorf("plugin %q (%s): %w", pluginID, stage, ErrExecutionTimeout)
		}
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

func isSubPath(rootReal string, targetReal string) bool {
	rootClean := filepath.Clean(rootReal)
	targetClean := filepath.Clean(targetReal)
	if targetClean == rootClean {
		return true
	}
	return strings.HasPrefix(targetClean, rootClean+string(filepath.Separator))
}
