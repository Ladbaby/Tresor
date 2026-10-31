// Tresor trusted synchronous CommonJS package contract (API V2 and V3).
// Importable host modules, each gated by a manifest feature:
//   tresor/json.v1      requires "json.lossless.v1"
//   tresor/query.v1     requires "query.v1"
//   tresor/encoding.v1  requires "encoding.v1"
//   tresor/buffer.v1    requires "buffer.v1"
//   tresor/kv.v1        requires "kv.v1"
// Importing a module whose feature the manifest does not declare fails package
// load. API V2 packages may declare a config_schema; resolved defaults (see
// context.limits for host-selected bounds) arrive as input.config.
export type Payload = {encoding:'utf8'|'base64';data:string};
export type Headers = Readonly<Record<string,readonly string[]>>;
export type Request = Readonly<{method:string;path:string;query:string;headers:Headers}>;
export type Response = Readonly<{status:number;headers:Headers}>;
export type Event = {type:string;data:string;id:string|null;retry:number|null};
export type Common = {readonly config:Readonly<Record<string,unknown>>;state:Record<string,unknown>;readonly attempt:Readonly<{index:number}>};
export type Limits = Readonly<{bodyBytes:number;eventBytes:number;stateBytes:number;bufferBytes:number;bufferHandles:number;bufferFragments:number;events:number;wireBytes:number;kvKeyBytes:number;kvValueBytes:number;kvKeysPerNamespace:number;kvTTLMs:number}>;
export type Failure = Readonly<{category:string;code:string;message:string;stage:string;plugin?:string;hook?:string;status?:number}>;
export type Statistics = Readonly<{rawBytes:number;rawEvents:number;outputBytes:number;outputEvents:number;collectedBytes:number}>;
export type Context = Readonly<{
 phase:'request'|'response_headers'|'events'|'response'|'finish';
 stage:'request'|'upstream_events'|'response_pipeline'|'collection'|'finish';
 formats:Readonly<{client:string|null;downstream:string|null;current:string|null}>;
 transport:Readonly<{carrier:'http';client:'buffered'|'streaming';requested:'auto'|'sse';observed:'unknown'|'buffered'|'sse';reportedContentType:string|null;classification:'unknown'|'header'|'declared'|'host';plannedDelivery:'buffered'|'streaming'|'collect';effectiveDelivery:'unknown'|'buffered'|'streaming'|'collect';collector:string|null}>;
 limits:Limits;
}>;
export type Metadata = Common & {readonly context:Context};
export type HeaderPatch = {setHeaders?:Record<string,string[]>;deleteHeaders?:string[]};
export type RequestInput = Metadata & {readonly request:Request;readonly body:Readonly<Payload>};
export type ResponseInput = Metadata & {readonly request:Request;readonly response:Response};
export type Termination = Readonly<{reason:string;error:Failure|null;statistics:Statistics}>;
export type EndInput = Metadata & {readonly end:Termination};
// Host-selected collection profiles preserve 64 MiB through response transforms.
// Ordinary state stays small; buffer contents are accounted incrementally.
// Byte limits are safeguards, not a hard memory sandbox.
// Initialization and hooks have no execution timer; authors must ensure they terminate.
// Integration hooks require api_version: 3. Hooks are synchronous and return
// declarative service requests only.
export type Routing = Readonly<{requestedModel:string;alias:null|{id:string;inputModel:string;isRegex:boolean};resolvedModel:string;downstream:{id:string;format:string}}>;
export type Execution = Readonly<{id:string;subject:string;assignment:string;epoch:number}>;
export type HostTool = Readonly<{name:string;description:string;parameters:object;operation:'execute-tool'}>;
export type PrepareInput = Readonly<{config:Readonly<Record<string,unknown>>;bindings?:Readonly<Record<string,Readonly<{binding_id:string}>>>;context:Context;routing:Routing;execution:Execution|null;request:Request;body:Payload}>;
export type Preparation = Readonly<{service:string;operation:'prepare';payload:unknown}>;
export type Decision = Readonly<{kind:'pass'}|{kind:'intercept';service:string;operation:'invoke';payload:unknown}|{kind:'managedTools';service:string;codec:'openai-chat.v1';instructions:string;tools:readonly HostTool[]}>;
export interface Plugin {
 // API V3 only; standard transform hooks below are available in V2 and V3.
 prepare?(input:PrepareInput):Preparation|{kind:'pass'};
 prepared?(input:PrepareInput&{result:unknown}):Decision;
 request?(input:RequestInput):void|{body?:Payload;request?:HeaderPatch & {method?:string;path?:string;query?:string};transport?:{upstream?:'sse';delivery?:'collect'}};
 responseHeaders?(input:ResponseInput):void|{response?:HeaderPatch};
 response?(input:ResponseInput & {readonly body:Readonly<Payload>}):void|{response?:HeaderPatch;body?:Payload};
 upstreamStart?(input:ResponseInput):void|{events:Event[]};
 upstreamEvent?(input:Metadata & {readonly event:Readonly<Event>}):void|{events:Event[]};
 upstreamEnd?(input:EndInput):void|{events:Event[]};
 streamStart?(input:ResponseInput):void|{events:Event[]};
 streamEvent?(input:Metadata & {readonly event:Readonly<Event>}):void|{events:Event[]};
 streamEnd?(input:EndInput):void|{events:Event[]};
 collectStart?(input:ResponseInput):void;
 collectEvent?(input:Metadata & {readonly event:Readonly<Event>}):void;
 collectEnd?(input:EndInput):void|{body:Payload;response?:HeaderPatch};
 finish?(input:Metadata & {readonly outcome:Termination}):void;
}
// require('tresor/buffer.v1'); stringifyLossless supports buffer string leaves.
export interface TextBuffers {
 create():object; append(handle:object,text:string):void; byteLength(handle:object):number;
 materialize(handle:object):string; release(handle:object):void; isBuffer(handle:unknown):boolean;
}
// require('tresor/kv.v1'); manifest must declare "kv.v1". A bounded persistent
// key-value store that survives across requests (unlike state). The host
// namespaces keys by the plugin's manifest id, so JS never supplies a namespace.
// Values are strings; get returns undefined on miss.
//
// Persistence is debounced, so writes may lag by about a second and a hard crash
// can lose the most recent ones: treat this as a cache.
//
// Every rule step using the same plugin id shares one namespace and one
// kvKeysPerNamespace quota, so prefix keys by purpose (for example "ses:") to
// keep independent uses from evicting each other. The host does not enforce
// prefixes. Ceilings are published in context.limits.
//
// getOrSet returns the existing value or stores and returns the supplied one in
// a single atomic step: use it, not get-then-set, whenever one value must be
// agreed on by concurrent requests (such as a stable per-conversation id).
// set(key, value, ttlMs?) — ttlMs 0 means no expiry.
export interface KV {
 get(key:string):string|undefined; set(key:string,value:string,ttlMs?:number):void;
 getOrSet(key:string,value:string,ttlMs?:number):string; del(key:string):boolean;
}
