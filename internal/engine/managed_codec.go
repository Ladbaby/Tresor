package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Codec names one client wire format for the managed chat path. A codec owns
// everything that differs between formats: how a request is read into the
// canonical transcript, and how a completed turn is rendered on the wire. The
// managed loop, the durable segment store, projection and replay stay codec
// agnostic — they operate on the canonical Chat form.
//
// Codecs never decide the transport. The client's stream flag does.
type Codec interface {
	// Name is the wire contract identity announced in the decision and stored
	// with a managed inference, so a replay can be rejected against another
	// format.
	Name() string
	// Format is the engine format id (openai, anthropic, openai_responses,
	// gemini) used for detection, rule matching and downstream translation.
	Format() string
	// Decode reads a client request into the canonical transcript.
	Decode(raw []byte, path string) (DecodedRequest, error)
	// AcceptsOpaque reports whether a provider-echoed block of this kind is
	// legal for the format, and validates its shape. Opaque state is stored and
	// echoed verbatim; it is never interpreted or regenerated.
	AcceptsOpaque(key string, value json.RawMessage) bool
	// Renderer supplies the wire frames for a streamed response.
	Renderer() StreamRenderer
	// ClientCallID rewrites a tool call's id into the id this wire format can
	// reproduce on a later turn. occurrence counts earlier calls in the same
	// turn with identical name and arguments, so two identical calls receive
	// distinct ids. Chat, Anthropic and Responses each echo a call
	// id verbatim, so they keep the downstream's id. Gemini carries no id at all
	// — a function response names only the function — so the id the loop records
	// must be one this codec will derive again, otherwise the following request
	// cannot be matched to its call. Returning the id unchanged is the default.
	ClientCallID(call ToolCall, occurrence int) string
}

// StreamRenderer turns the streamer's canonical events into wire frames for one
// client format. The streamer owns when to emit (progress, answer, terminal);
// the renderer owns what bytes represent them.
type StreamRenderer interface {
	// FrameStart opens the stream (e.g. Anthropic's message_start). May be nil.
	FrameStart() []byte
	// FrameProgress renders a waiting/status delta. Warmup text is the initial
	// status already carried by FrameStart, when the format has one.
	FrameProgress(text string) []byte
	// FrameContent renders answer text.
	FrameContent(text string) []byte
	// FrameFinish renders the terminal turn, including usage when present.
	FrameFinish(usage json.RawMessage) []byte
	// FrameEnd is the format's end-of-stream marker, nil when the format has none.
	FrameEnd() []byte
	// FrameError renders a post-commit failure.
	FrameError(code string) []byte
}

// streamLifecycle is implemented by renderers whose framing must be driven
// explicitly rather than inferred from content frames. Chat needs neither: its
// delta frames are self-describing. Anthropic requires a message_start before
// any block, an open/close around each content block, and a message_delta after
// the last one, so the streamer consults these hooks at each transition.
type streamLifecycle interface {
	// FrameBlocks reports whether content frames require an opened block.
	FrameBlocks() bool
	// FrameOpenBlock opens the block carrying the given kind ("progress" or
	// "content"), closing whichever block was open. Nil when unsupported.
	FrameOpenBlock(kind string) []byte
	// FrameCloseBlock closes the block currently open, if any.
	FrameCloseBlock() []byte
}

// streamToolCaller is implemented by renderers that can carry a tool call on the
// streamed wire. A managed turn may end by asking the client to run tools rather
// than with prose, and a client that cannot see those calls cannot continue the
// loop, so the calls must be rendered rather than dropped.
type streamToolCaller interface {
	// FrameToolCalls renders the calls a stored turn requested, before the
	// terminal frame.
	FrameToolCalls(calls []ToolCall) []byte
}

// streamTerminator is implemented by renderers that must write a closing turn
// even when the answer already streamed live, because the format carries the
// stop reason in that frame. Chat is not one: its deltas are self-describing and
// its historical wire omits the finish chunk on the live path. This is a
// separate interface from streamLifecycle because a format can terminate
// explicitly without framing its content in blocks (Gemini does).
type streamTerminator interface {
	FrameAlwaysTerminates() bool
}

// jsonResponseRenderer is a StreamRenderer that can also render the format's
// non-streaming JSON body from a stored canonical response. A renderer that
// does not implement it must never fall back to another format's bytes.
type jsonResponseRenderer interface {
	RenderJSON(canonical []byte) ([]byte, error)
}

// DecodedRequest is a validated client request in canonical form.
type DecodedRequest struct {
	// Messages are the canonical (OpenAI Chat) messages.
	Messages []ChatMessage
	// Model is the model the client asked for.
	Model string
	// Stream is the client's transport preference.
	Stream bool
	// Raw carries the request fields the loop forwards upstream, already
	// expressed under canonical (OpenAI Chat) names. A codec must not copy
	// format-specific field names here: the loop sends Raw to the downstream,
	// so a foreign key would surface as an unknown parameter.
	Raw map[string]json.RawMessage
}

// codecPaths maps a request path to the codec that serves it. Every format the
// gateway can detect must appear here, otherwise a managed request on that
// path is rejected explicitly rather than silently mis-served.
var codecPaths = map[string]string{
	"/v1/chat/completions": "openai-chat.v1",
	"/chat/completions":    "openai-chat.v1",
	"/v1/messages":         "anthropic-messages.v1",
	"/v1/responses":        "openai-responses.v1",
}

const geminiPathPrefix = "/v1beta/models/"

func init() {
	registerCodec(&openAIChatCodec{})
	registerCodec(&anthropicCodec{})
	registerCodec(&responsesCodec{})
	registerCodec(&geminiCodec{})
}

var codecsByPath = map[string]Codec{}
var codecsByName = map[string]Codec{}

func registerCodec(c Codec) {
	codecsByName[c.Name()] = c
	for path, name := range codecPaths {
		if name != c.Name() {
			continue
		}
		if _, taken := codecsByPath[path]; taken {
			panic("engine: two codecs claim " + path)
		}
		codecsByPath[path] = c
	}
}

const geminiCodecName = "gemini-generate-content.v1"

// codecForPath resolves the codec serving a request path.
func codecForPath(path string) (Codec, bool) {
	if c, ok := codecsByPath[path]; ok {
		return c, true
	}
	if strings.HasPrefix(path, geminiPathPrefix) {
		if c, ok := codecsByName[geminiCodecName]; ok {
			return c, true
		}
	}
	return nil, false
}

// codecForName resolves an announced codec. A decision naming an unknown codec
// is a contract error, never a silent fallback to Chat.
func codecForName(name string) (Codec, bool) {
	c, ok := codecsByName[name]
	return c, ok
}

// managedCodecNames lists every registered codec, for diagnostics and tests.
func managedCodecNames() []string {
	out := make([]string, 0, len(codecsByName))
	for name := range codecsByName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// opaqueKeyAllowed is the codec-specific gate for provider-echoed state: a
// block is either plain text the loop understands, or a shape the current codec
// explicitly accepts. Formats whose specs carry provider-echoed blocks (thinking
// signatures, encrypted reasoning) wire this into their decode path; the native
// Chat format keeps the blanket rejection in hasOpaque.
func opaqueKeyAllowed(codec Codec, key string, value json.RawMessage) bool {
	if codec == nil {
		return false
	}
	if textualReasoning(key, value) {
		return true
	}
	return codec.AcceptsOpaque(key, value)
}

// decodeClientRequest reads a client request through its codec.
func decodeClientRequest(codec Codec, raw []byte, path string) (DecodedRequest, error) {
	if codec == nil {
		return DecodedRequest{}, fmt.Errorf("%w: no codec for route", ErrManagedChat)
	}
	decoded, err := codec.Decode(raw, path)
	if err != nil {
		return DecodedRequest{}, err
	}
	if len(decoded.Messages) == 0 {
		return DecodedRequest{}, fmt.Errorf("%w: messages required", ErrManagedChat)
	}
	return decoded, nil
}
