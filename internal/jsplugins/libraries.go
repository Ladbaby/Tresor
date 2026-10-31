package jsplugins

// moduleDescriptor describes one importable tresor/*.v1 module. Source is the
// module body; Feature is the manifest feature a package must declare to import
// it. Keeping both in one entry means a new capability cannot be added
// half-wired: the allowlist, the import gate, and the injected natives all read
// from this table.
type moduleDescriptor struct {
	Source  string
	Feature string
}

// hostModules is the single source of truth for importable modules. Versioned
// modules have no engine access: JSON token helpers are bounded, pure host
// operations, and plugins receive no filesystem or network capabilities.
var hostModules = map[string]moduleDescriptor{
	"tresor/json.v1":     {losslessJSON, "json.lossless.v1"},
	"tresor/query.v1":    {queryLibrary, "query.v1"},
	"tresor/encoding.v1": {encodingLibrary, "encoding.v1"},
	"tresor/buffer.v1":   {`module.exports=__tresorBuffers;`, "buffer.v1"},
	"tresor/kv.v1": {`module.exports=Object.freeze({get:k=>__tresorKVGet(k),` +
		`set:(k,v,t)=>__tresorKVSet(k,v,t===undefined?0:t),` +
		`getOrSet:(k,v,t)=>__tresorKVGetOrSet(k,v,t===undefined?0:t),` +
		`del:k=>__tresorKVDelete(k)});`, "kv.v1"},
}

// capabilityFeatures are manifest features that gate host behaviour rather than
// a module: SSE hook families, an option inside tresor/json.v1, and the API V3
// integration protocols. They are valid in "requires" but import nothing.
var capabilityFeatures = []string{
	"sse.events.v2", "sse.collect.v2", "sse.upstream.v2",
	"json.patch.v1",
	"integration.prepare.v1", "integration.intercept.v1", "tools.host.v1",
}

// gatedFeature reports the manifest feature a module requires. The second result
// is false for an unknown spec.
func gatedFeature(spec string) (string, bool) {
	d, ok := hostModules[spec]
	if !ok {
		return "", false
	}
	return d.Feature, true
}

const losslessJSON = `
const tokens=new WeakMap();
class NumberToken {constructor(raw){if(typeof raw!=='string'||!/^[-]?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$/.test(raw))throw Error('invalid number token');tokens.set(this,raw);__tresorOpaqueSizes.set(this,__tresorUTF8(raw)+16);Object.freeze(this);}toString(){return tokens.get(this);}valueOf(){throw Error('explicit numeric conversion required');}}
function parseLossless(text){
 let p=0;function ws(){while(p<text.length&&(text[p]===' '||text[p]==='\t'||text[p]==='\r'||text[p]==='\n'))p++;}
 // Decode only string tokens through the bounded host helper. Numeric tokens
 // remain lossless; UTF-16 offsets and lone surrogates retain JS semantics.
 function str(){const token=__tresorJSONString(text,p);p=token[0];return token[1];}
 function value(){ws();const c=text[p];if(c==='"')return str();if(c==='['){p++;const a=[];ws();if(text[p]===']'){p++;return a;}for(;;){a.push(value());ws();const end=text[p++];if(end===']')return a;if(end!==',')throw Error('invalid array');}}
 if(c==='{'){p++;const o={};ws();if(text[p]==='}'){p++;return o;}for(;;){ws();if(text[p]!=='"')throw Error('invalid key');const k=str();ws();if(text[p++]!==':')throw Error('missing colon');Object.defineProperty(o,k,{value:value(),writable:true,enumerable:true,configurable:true});ws();const end=text[p++];if(end==='}')return o;if(end!==',')throw Error('invalid object');}}
 for(const [word,v]of [['true',true],['false',false],['null',null]])if(text.slice(p,p+word.length)===word){p+=word.length;return v;}
 const raw=__tresorJSONNumber(text,p);p+=raw.length;
 // Preserve every source number spelling, including decimals and exponents.
 return new NumberToken(raw);
 }
 const out=value();ws();if(p!==text.length)throw Error('trailing JSON');return out;
}
function stringifyLossless(value){const seen=new Set();function write(v){if(__tresorBuffers.isBuffer(v))return __tresorQuote(__tresorBuffers.materialize(v));if(tokens.has(v))return tokens.get(v);if(v===null)return 'null';if(typeof v==='string'||typeof v==='boolean')return JSON.stringify(v);if(typeof v==='number'){if(!Number.isFinite(v))throw Error('nonfinite number');return JSON.stringify(v);}if(typeof v!=='object')throw Error('non-JSON value');if(seen.has(v))throw Error('cyclic JSON');seen.add(v);let out;if(Array.isArray(v))out='['+v.map(write).join(',')+']';else out='{'+Object.keys(v).map(k=>JSON.stringify(k)+':'+write(v[k])).join(',')+'}';seen.delete(v);return out;}return write(value);}
function toNumber(v){if(typeof v==='number'){if(!Number.isFinite(v))throw Error('nonfinite number');return v;}if(!tokens.has(v))throw Error('expected number token');const raw=tokens.get(v),n=Number(raw);if(!Number.isFinite(n)||!Number.isSafeInteger(n)||!/^[-]?[0-9]+$/.test(raw))throw Error('unsafe number conversion');return n;}
// RFC 6902 subset: add/remove/replace; JSON-pointer paths; '-' only for array
// append in add; remove shifts arrays, replace/remove require an existing key.
function patch(text,operations){const root=parseLossless(text);let result=root;for(const op of operations){if(!['add','remove','replace'].includes(op.op)||typeof op.path!=='string')throw Error('unsupported patch');if(op.path===''){if(op.op==='remove')throw Error('cannot remove root');result=op.value;continue;}if(op.path[0]!=='/')throw Error('invalid pointer');const keys=op.path.slice(1).split('/').map(k=>{if(/~(?![01])/.test(k))throw Error('invalid pointer escape');return k.replace(/~1/g,'/').replace(/~0/g,'~');});let parent=result;for(const k of keys.slice(0,-1)){if(!parent||typeof parent!=='object'||!Object.prototype.hasOwnProperty.call(parent,k))throw Error('missing pointer');parent=parent[k];}const k=keys[keys.length-1];if(!parent||typeof parent!=='object'||tokens.has(parent))throw Error('invalid pointer parent');if(Array.isArray(parent)){const index=k==='-'&&op.op==='add'?parent.length:/^(0|[1-9][0-9]*)$/.test(k)?Number(k):-1;if(index<0||index>parent.length||op.op!=='add'&&index===parent.length)throw Error('invalid array index');if(op.op==='add')parent.splice(index,0,op.value);else if(op.op==='remove')parent.splice(index,1);else parent[index]=op.value;}else{if(op.op!=='add'&&!Object.prototype.hasOwnProperty.call(parent,k))throw Error('missing key');if(op.op==='remove')delete parent[k];else Object.defineProperty(parent,k,{value:op.value,writable:true,enumerable:true,configurable:true});}}
 return stringifyLossless(result);}
module.exports=Object.freeze({parseLossless,stringifyLossless,toNumber,NumberToken,patch});
`
const queryLibrary = `
function parse(query){if(query==='')return [];return query.split('&').map(part=>{const pos=part.indexOf('=');const decode=s=>decodeURIComponent(s.replace(/\+/g,' '));return [decode(pos<0?part:part.slice(0,pos)),decode(pos<0?'':part.slice(pos+1))];});}
function stringify(entries){return entries.map(([key,value])=>encodeURIComponent(key)+'='+encodeURIComponent(value)).join('&');}
module.exports=Object.freeze({parse,stringify});
`
const encodingLibrary = `
const alphabet='ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';
function decodeBase64(text){if(!/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(text))throw Error('invalid base64');const out=[];for(let p=0;p<text.length;p+=4){const a=alphabet.indexOf(text[p]),b=alphabet.indexOf(text[p+1]),c=alphabet.indexOf(text[p+2]),d=alphabet.indexOf(text[p+3]);out.push((a<<2)|(b>>4));if(text[p+2]!=='=')out.push(((b&15)<<4)|(c>>2));if(text[p+3]!=='=')out.push(((c&3)<<6)|d);}return out;}
function encodeBase64(bytes){let out='';for(let p=0;p<bytes.length;p+=3){const a=bytes[p],b=bytes[p+1],c=bytes[p+2];out+=alphabet[a>>2]+alphabet[((a&3)<<4)|((b||0)>>4)]+(b===undefined?'=':alphabet[((b&15)<<2)|((c||0)>>6)])+(c===undefined?'=':alphabet[c&63]);}return out;}
function decode(body){if(body.encoding==='utf8')return body.data;if(body.encoding!=='base64')throw Error('unknown encoding');const bytes=decodeBase64(body.data);let escaped='';for(const byte of bytes)escaped+='%'+byte.toString(16).padStart(2,'0');return decodeURIComponent(escaped);}
function encode(text){return {encoding:'utf8',data:text};}
module.exports=Object.freeze({decode,encode,decodeBase64,encodeBase64});
`
