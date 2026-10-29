package jsplugins

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"sync"
	"tresor/internal/integrations"

	"github.com/dop251/goja"
)

type Instance struct {
	pkg          *Package
	config       map[string]any
	vm           *goja.Runtime
	exports      *goja.Object
	invoke       goja.Callable
	declared     map[string]bool
	limits       Limits
	detachedBody map[string]any
	invoking     bool
	hostFault    string
	mu           sync.Mutex
}

func (i *Instance) SourceIdentity() (string, string) { return i.pkg.Manifest.ID, i.pkg.Digest }
func (i *Instance) AllowsService(service, operation string) bool {
	decl, declared := i.pkg.Manifest.Services[service]
	_, configured := decl.Operations[integrations.Operation(operation)]
	return i.pkg.Manifest.APIVersion == 3 && contains(i.pkg.Manifest.Permissions, "service."+operation) && declared && configured
}

// fault enforces native host contract violations independently of telemetry.
func (i *Instance) fault(code string) {
	if i.hostFault == "" {
		i.hostFault = code
	}
	panic(i.vm.NewTypeError("Host contract violation"))
}

func NewInstance(pkg *Package, config map[string]any) (*Instance, error) {
	return newInstance(pkg, config, true)
}
func newInstance(pkg *Package, config map[string]any, validate bool) (*Instance, error) {
	if !pkg.Compatible {
		return nil, fmt.Errorf("incompatible API: require V2 migration")
	}
	if pkg.Manifest.APIVersion != 2 && pkg.Manifest.APIVersion != 3 {
		return nil, fmt.Errorf("unsupported API version")
	}
	resolved := map[string]any{}
	if validate {
		var err error
		resolved, err = ResolveConfig(pkg.Manifest.ConfigSchema, config)
		if err != nil {
			return nil, err
		}
	}
	instance := &Instance{pkg: pkg, config: resolved}
	if validate {
		for _, cfg := range instance.ServiceConfigs() {
			if _, err := integrations.NewConfiguredClient(cfg.Service, cfg.Token, cfg.ControlToken, 8<<20); err != nil {
				return nil, fmt.Errorf("invalid service parameters")
			}
			if cfg.ControlToken != "" && cfg.ControlToken == cfg.Token {
				return nil, fmt.Errorf("control and outbound credentials must differ")
			}
		}
	}
	if err := instance.Reset(); err != nil {
		return nil, err
	}
	return instance, nil
}

// decodeJSONString scans a bounded ECMAScript string without converting numbers
// or replacing unpaired UTF-16 surrogates. Callers validate the opening quote.
func decodeJSONString(text goja.String, start int) (int, goja.String, error) {
	units := make([]uint16, 0, 64)
	for p := start + 1; p < text.Length(); p++ {
		c := text.CharAt(p)
		switch {
		case c == '"':
			return p + 1, goja.StringFromUTF16(units), nil
		case c < 0x20:
			return 0, nil, fmt.Errorf("unescaped control character")
		case c == '\\':
			p++
			if p >= text.Length() {
				return 0, nil, fmt.Errorf("unfinished escape")
			}
			switch text.CharAt(p) {
			case '"', '\\', '/':
				c = text.CharAt(p)
			case 'b':
				c = '\b'
			case 'f':
				c = '\f'
			case 'n':
				c = '\n'
			case 'r':
				c = '\r'
			case 't':
				c = '\t'
			case 'u':
				c = 0
				for n := 0; n < 4; n++ {
					p++
					if p >= text.Length() {
						return 0, nil, fmt.Errorf("unfinished unicode escape")
					}
					h := text.CharAt(p)
					switch {
					case h >= '0' && h <= '9':
						h -= '0'
					case h >= 'a' && h <= 'f':
						h = h - 'a' + 10
					case h >= 'A' && h <= 'F':
						h = h - 'A' + 10
					default:
						return 0, nil, fmt.Errorf("invalid unicode escape")
					}
					c = c*16 + h
				}
			default:
				return 0, nil, fmt.Errorf("invalid escape")
			}
		}
		units = append(units, c)
	}
	return 0, nil, fmt.Errorf("unterminated string")
}

func (i *Instance) PluginName() string { return i.pkg.Manifest.ID + "@" + i.pkg.Manifest.Version }
func (i *Instance) Reset() (result error) {
	defer func() {
		if result != nil {
			result = &InvocationError{Code: "initialization_failed", Plugin: i.PluginName(), Hook: "initialize", Cause: result}
		}
	}()
	i.mu.Lock()
	defer i.mu.Unlock()
	i.vm = goja.New()
	i.invoking = false
	i.hostFault = ""
	i.limits = EffectiveLimits(false)
	i.vm.SetMaxCallStackSize(512)
	// Initialization is deliberately untimed, like plugin hook execution.
	return func() error {
		// Every value crossing the boundary is JSON-detached. No reflection or live
		// Go objects are exposed to source; plugin authors control execution time.
		_, err := i.vm.RunString(runtimePrelude)
		if err != nil {
			return err
		}
		i.vm.Set("__tresorFaultNative", func(call goja.FunctionCall) goja.Value {
			code, ok := call.Argument(0).Export().(string)
			if !ok {
				i.fault("invalid_effect")
			}
			switch code {
			case "buffer_bytes", "buffer_handles", "buffer_fragments", "state_bytes", "input_limit", "invalid_effect", "permission_denied", "log_limit":
			default:
				code = "invalid_effect"
			}
			i.fault(code)
			return goja.Undefined()
		})
		// Decode only string tokens natively. No JSON numbers cross this boundary;
		// UTF-16 code units preserve lone surrogates and JS JSON.parse semantics.
		i.vm.Set("__tresorJSONString", func(call goja.FunctionCall) goja.Value {
			text, ok := call.Argument(0).(goja.String)
			start := call.Argument(1).ToInteger()
			if !ok || text.Length() > MaxCollectionBytes || start < 0 || start >= int64(text.Length()) || text.CharAt(int(start)) != '"' {
				panic(i.vm.NewTypeError("invalid JSON string input"))
			}
			end, decoded, err := decodeJSONString(text, int(start))
			if err != nil {
				panic(i.vm.NewTypeError("invalid JSON string"))
			}
			return i.vm.NewArray(end, decoded)
		})
		i.vm.Set("__tresorJSONNumber", func(call goja.FunctionCall) goja.Value {
			text, ok := call.Argument(0).(goja.String)
			start := int(call.Argument(1).ToInteger())
			if !ok || text.Length() > MaxCollectionBytes || start < 0 || start >= text.Length() {
				panic(i.vm.NewTypeError("invalid JSON number input"))
			}
			p := start
			for p < text.Length() {
				c := text.CharAt(p)
				if (c >= '0' && c <= '9') || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' {
					p++
				} else {
					break
				}
			}
			if p == start {
				panic(i.vm.NewTypeError("invalid JSON number"))
			}
			return text.Substring(start, p)
		})
		i.vm.Set("__tresorQuote", func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) != 1 {
				panic(i.vm.NewTypeError("quote expects text"))
			}
			s, ok := call.Argument(0).Export().(string)
			if !ok || len(s) > MaxCollectionBytes {
				panic(i.vm.NewTypeError("invalid quote input"))
			}
			data, _ := json.Marshal(s)
			return i.vm.ToValue(string(data))
		})
		i.vm.Set("__tresorDetachBody", func(call goja.FunctionCall) goja.Value {
			encoding, ok := call.Argument(0).Export().(string)
			if !ok || (encoding != "utf8" && encoding != "base64") {
				i.fault("invalid_effect")
			}
			data, ok := call.Argument(1).Export().(string)
			limit := i.limits.BodyBytes
			if encoding == "base64" {
				limit = (limit + 2) / 3 * 4
			}
			if !ok || len(data) > limit {
				i.fault("input_limit")
			}
			i.detachedBody = map[string]any{"encoding": encoding, "data": data}
			return goja.Undefined()
		})
		modules := map[string]goja.Value{}
		var load func(string) (goja.Value, error)
		load = func(name string) (goja.Value, error) {
			if value, ok := modules[name]; ok {
				return value, nil
			}
			program, ok := i.pkg.programs[name]
			if !ok {
				source, exists := libraries[name]
				if !exists {
					return nil, fmt.Errorf("module not available: %s", name)
				}
				var err error
				program, err = goja.Compile(name, "(function(module,exports,require){\"use strict\";\n"+source+"\n})", true)
				if err != nil {
					return nil, err
				}
			}
			module := i.vm.NewObject()
			exports := i.vm.NewObject()
			module.Set("exports", exports)
			modules[name] = exports
			value, err := i.vm.RunProgram(program)
			if err != nil {
				return nil, err
			}
			factory, ok := goja.AssertFunction(value)
			if !ok {
				return nil, fmt.Errorf("invalid module")
			}
			require := func(call goja.FunctionCall) goja.Value {
				if len(call.Arguments) != 1 || call.Argument(0).ExportType() == nil {
					panic(i.vm.NewTypeError("require expects module name"))
				}
				spec := call.Argument(0).String()
				resolved := spec
				if strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") {
					resolved = path.Clean(path.Join(path.Dir(name), spec))
					if !safeFile(resolved) {
						panic(i.vm.NewTypeError("unsafe module path"))
					}
					if _, ok := i.pkg.programs[resolved]; !ok {
						resolved += ".js"
					}
				} else if _, ok := libraries[spec]; !ok {
					panic(i.vm.NewTypeError("forbidden import: %s", spec))
				}
				out, err := load(resolved)
				if err != nil {
					panic(i.vm.NewGoError(err))
				}
				return out
			}
			if _, err := factory(goja.Undefined(), module, exports, i.vm.ToValue(require)); err != nil {
				return nil, err
			}
			out := module.Get("exports")
			modules[name] = out
			return out, nil
		}
		exports, err := load(i.pkg.Manifest.Entry)
		if err != nil {
			return fmt.Errorf("%s loading: %w", i.PluginName(), err)
		}
		if goja.IsNull(exports) || goja.IsUndefined(exports) {
			return fmt.Errorf("entry must export hook object")
		}
		i.exports = exports.ToObject(i.vm)
		if i.exports.ClassName() != "Object" {
			return fmt.Errorf("entry must export hook object")
		}
		i.declared = map[string]bool{}
		for _, name := range i.exports.Keys() {
			if !hooks[name] {
				return fmt.Errorf("unknown hook %q", name)
			}
			if _, ok := goja.AssertFunction(i.exports.Get(name)); !ok {
				return fmt.Errorf("hook %s must be a function", name)
			}
			i.declared[name] = true
		}
		freeze, _ := goja.AssertFunction(i.vm.Get("__tresorFreeze"))
		if _, err := freeze(goja.Undefined(), i.exports); err != nil {
			return err
		}
		init, _ := goja.AssertFunction(i.vm.Get("__tresorInit"))
		if _, err := init(goja.Undefined()); err != nil {
			return err
		}
		if i.hostFault != "" {
			return fmt.Errorf("module loading violated host contract")
		}
		i.invoke, _ = goja.AssertFunction(i.vm.Get("__tresorCheckedInvoke"))
		return nil
	}()
}
func (i *Instance) HasHook(name string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.declared[name]
}
func (i *Instance) Invoke(name string, input map[string]any) (map[string]any, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.declared[name] {
		if name == "finish" {
			i.releaseBuffers()
		}
		return nil, nil
	}
	limits := i.limits
	i.invoking = true
	i.hostFault = ""
	defer func() { i.invoking = false }()
	i.detachedBody = nil
	inputCopy := make(map[string]any, len(input))
	for k, v := range input {
		if k != "body" {
			inputCopy[k] = v
		}
	}
	inputCopy["config"] = i.config
	var inputBody goja.Value = goja.Undefined()
	if raw, ok := input["body"]; ok {
		b, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid body input")
		}
		text, ok := b["data"].(string)
		bodyLimit := limits.BodyBytes
		if b["encoding"] == "base64" {
			bodyLimit = (bodyLimit + 2) / 3 * 4
		} else if b["encoding"] != "utf8" {
			return nil, fmt.Errorf("invalid input encoding")
		}
		if !ok || len(text) > bodyLimit {
			return nil, fmt.Errorf("body input exceeds limit")
		}
		obj := i.vm.NewObject()
		obj.Set("encoding", b["encoding"])
		obj.Set("data", text)
		inputBody = obj
	}
	if ev, ok := input["event"].(map[string]any); ok {
		if s, ok := ev["data"].(string); !ok || len(s) > limits.EventBytes {
			return nil, fmt.Errorf("event input exceeds limit")
		}
	}
	data, err := json.Marshal(inputCopy)
	if err != nil || len(data) > limits.WireBytes {
		return nil, &InvocationError{Code: "input_limit", Plugin: i.PluginName(), Hook: name, Cause: fmt.Errorf("input exceeds limit")}
	}
	var result map[string]any
	err = func() error {
		out, err := i.invoke(goja.Undefined(), i.exports, i.vm.ToValue(name), i.vm.ToValue(string(data)), i.vm.ToValue(limits.WireBytes), i.vm.ToValue(limits.StateBytes), i.vm.ToValue(limits.Events), inputBody)
		if err != nil {
			return err
		}
		if goja.IsUndefined(out) {
			return nil
		}
		text := out.String()
		if len(text) > limits.WireBytes {
			return fmt.Errorf("result exceeds limit")
		}
		return json.Unmarshal([]byte(text), &result)
	}()
	if name == "finish" {
		release, _ := goja.AssertFunction(i.vm.Get("__tresorReleaseBuffers"))
		if release != nil {
			_, _ = release(goja.Undefined())
		}
	}
	if i.hostFault != "" {
		return nil, &InvocationError{Code: i.hostFault, Plugin: i.PluginName(), Hook: name, Cause: fmt.Errorf("host contract violation")}
	}
	if err != nil {
		return nil, &InvocationError{Code: "plugin_failure", Plugin: i.PluginName(), Hook: name, Cause: err}
	}
	if code, ok := result["__hostFailure"].(string); ok {
		return nil, &InvocationError{Code: code, Plugin: i.PluginName(), Hook: name, Cause: fmt.Errorf("hook contract failed")}
	}
	if i.detachedBody != nil {
		if result == nil {
			result = map[string]any{}
		}
		result["body"] = i.detachedBody
		i.detachedBody = nil
	}
	return result, nil
}

func (i *Instance) releaseBuffers() {
	release, _ := goja.AssertFunction(i.vm.Get("__tresorReleaseBuffers"))
	if release != nil {
		_, _ = release(goja.Undefined())
	}
}
func (i *Instance) ReleaseBuffers() { i.mu.Lock(); defer i.mu.Unlock(); i.releaseBuffers() }
func (i *Instance) SetLimits(limits Limits) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.limits = limits
	set, _ := goja.AssertFunction(i.vm.Get("__tresorSetBufferLimits"))
	if set != nil {
		_, _ = set(goja.Undefined(), i.vm.ToValue(limits.BufferBytes), i.vm.ToValue(limits.BufferHandles), i.vm.ToValue(limits.BufferFragments))
	}
}

const runtimePrelude = `
const __tresorJSONParse=JSON.parse, __tresorJSONStringify=JSON.stringify;
const __tresorKeys=Object.keys, __tresorFreeze=Object.freeze;
const __tresorHostFaults=new WeakMap();
function __tresorFault(code){__tresorFaultNative(code);}
function __tresorCheckedInvoke(...args){try{return __tresorInvoke(...args);}catch(e){return __tresorJSONStringify({__hostFailure:'plugin_exception'});}}
const __tresorOpaqueSizes=new WeakMap();
let __tresorBufferStore=new WeakMap(), __tresorBufferBytes=0, __tresorBufferCount=0, __tresorFragments=0;
let __tresorBufferLimit=8388608, __tresorHandleLimit=65536, __tresorFragmentLimit=1048576;
function __tresorUTF8(s){if(!/[^\x00-\x7f]/.test(s))return s.length;let n=0;for(let i=0;i<s.length;i++){const c=s.charCodeAt(i);if(c<128)n++;else if(c<2048)n+=2;else if(c>=55296&&c<=56319&&i+1<s.length&&s.charCodeAt(i+1)>=56320&&s.charCodeAt(i+1)<=57343){n+=4;i++;}else n+=3;}return n;}
function __tresorSetBufferLimits(bytes,handles,fragments){__tresorBufferLimit=bytes;__tresorHandleLimit=handles;__tresorFragmentLimit=fragments;}
function __tresorReleaseBuffers(){__tresorBufferStore=new WeakMap();__tresorBufferBytes=0;__tresorBufferCount=0;__tresorFragments=0;}
const __tresorBuffers=(()=>{
 const get=h=>{const b=__tresorBufferStore.get(h);if(!b)__tresorFault('invalid_effect');return b;};
 const create=()=>{if(__tresorBufferCount>=__tresorHandleLimit)__tresorFault('buffer_handles');const h=Object.freeze(Object.create(null));__tresorBufferStore.set(h,{parts:[],bytes:0,tail:0});__tresorOpaqueSizes.set(h,16);__tresorBufferCount++;return h;};
 const append=(h,s)=>{const b=get(h);if(typeof s!=='string')__tresorFault('invalid_effect');if(s==='')return;const first=s.charCodeAt(0),last=s.charCodeAt(s.length-1);const bytes=__tresorUTF8(s)-(b.tail>=55296&&b.tail<=56319&&first>=56320&&first<=57343?2:0);if(bytes>__tresorBufferLimit-__tresorBufferBytes)__tresorFault('buffer_bytes');if(__tresorFragments>=__tresorFragmentLimit)__tresorFault('buffer_fragments');b.parts.push(s);b.tail=last;b.bytes+=bytes;__tresorBufferBytes+=bytes;__tresorFragments++;};
 return Object.freeze({create,append,byteLength:h=>get(h).bytes,materialize:h=>get(h).parts.join(''),release:h=>{const b=get(h);__tresorBufferBytes-=b.bytes;__tresorFragments-=b.parts.length;__tresorBufferCount--;__tresorBufferStore.delete(h);},isBuffer:h=>__tresorBufferStore.has(h)});
})();
let __tresorState;
function __tresorDeepFreeze(x){ if(x && typeof x==='object'){for(const k of __tresorKeys(x))__tresorDeepFreeze(x[k]);__tresorFreeze(x);}return x; }
function __tresorInit(){__tresorState={};}
function __tresorStateSize(x){
 const seen=new Set();let total=0;
 const utf8=s=>{if(!/[^\x00-\x7f]/.test(s))return s.length;let n=0;for(let i=0;i<s.length;i++){const c=s.charCodeAt(i);if(c<128)n++;else if(c<2048)n+=2;else if(c>=55296&&c<=56319&&i+1<s.length&&s.charCodeAt(i+1)>=56320&&s.charCodeAt(i+1)<=57343){n+=4;i++;}else n+=3;}return n;};
 function walk(v){if(v && __tresorOpaqueSizes.has(v)){total+=__tresorOpaqueSizes.get(v);return;}if(v===null||typeof v==='boolean'||typeof v==='number'){total+=8;return;}if(typeof v==='string'){total+=utf8(v);return;}if(typeof v!=='object')__tresorFault('invalid_effect');if(seen.has(v))__tresorFault('invalid_effect');seen.add(v);total+=2;for(const k of __tresorKeys(v)){total+=utf8(k)+4;walk(v[k]);}seen.delete(v);}
 walk(x);return total;
}
function __tresorInvoke(exports,name,raw,limit,stateLimit,maxEvents,body){
 const parsed=__tresorJSONParse(raw);if(body!==undefined)parsed.body=body;
 const input=__tresorDeepFreeze(parsed);
 const args={...input,state:__tresorState};__tresorFreeze(args);
 let result;
 try{result=exports[name](args);}catch(e){if(__tresorStateSize(__tresorState)>stateLimit)__tresorFault('state_bytes');throw e;}
 if(__tresorStateSize(__tresorState)>stateLimit)__tresorFault('state_bytes');
 if(result===undefined)return undefined;
 try {
 if(!result || typeof result!=='object' || Array.isArray(result) || typeof result.then==='function')__tresorFault('invalid_effect');
 const allowed={prepare:['service','operation','payload'],prepared:['kind','service','operation','payload','codec','instructions','tools','stream'],request:['request','body','transport'],responseHeaders:['response'],response:['response','body'],upstreamStart:['events'],upstreamEvent:['events'],upstreamEnd:['events'],streamStart:['events'],streamEvent:['events'],streamEnd:['events'],collectStart:[],collectEvent:[],collectEnd:['body','response'],finish:[]}[name];
 for(const k of __tresorKeys(result))if(!allowed.includes(k))__tresorFault('invalid_effect');

 if(result.events!==undefined&&(!Array.isArray(result.events)||result.events.length>maxEvents))__tresorFault('invalid_effect');
 if(result.body!==undefined){const b=result.body;if(!b||typeof b!=='object'||Array.isArray(b)||__tresorKeys(b).length!==2||typeof b.data!=='string')__tresorFault('invalid_effect');__tresorDetachBody(b.encoding,b.data);}
 const detached={};for(const k of __tresorKeys(result))if(k!=='body')Object.defineProperty(detached,k,{value:result[k],enumerable:true});
 const text=__tresorJSONStringify(detached);if(text.length>limit)__tresorFault('input_limit');return text;
 }catch(e){__tresorFault('invalid_effect');}
}
// Privileged/browser/Node APIs are not provided. Promises are forbidden by the
// synchronous contract; module code cannot start asynchronous work.
globalThis.Promise=undefined;
`
