// Tresor V2 trusted synchronous CommonJS package contract.
// Import pure helpers via require('tresor/json.v1'), 'tresor/query.v1', or
// 'tresor/encoding.v1'. JSON patch supports add/remove/replace JSON pointers.
export type Payload = {encoding:'utf8'|'base64';data:string};
export type Headers = Readonly<Record<string,readonly string[]>>;
export type Request = Readonly<{method:string;path:string;query:string;headers:Headers}>;
export type Response = Readonly<{status:number;headers:Headers}>;
export type Event = {type:string;data:string;id:string|null;retry:number|null};
export type Common = {state:Record<string,unknown>;readonly attempt:Readonly<{index:number}>};
export type Limits = Readonly<{bodyBytes:number;eventBytes:number;stateBytes:number;bufferBytes:number;bufferHandles:number;bufferFragments:number;events:number;wireBytes:number}>;
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
export interface Plugin {
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
