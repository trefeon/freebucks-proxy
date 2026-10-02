package convert

// ir_registry.go — request/response registries with a quality + diagnostics
// vocabulary (Phase 1.1/1.4 of docs/UNIVERSAL-GATEWAY-PLAN.md).
//
// Relaykit precedent: "add a From/To spec" per surface; llmrelay precedent:
// two codecs per dialect. Each surface registers one request decoder and one
// response renderer; the gateway composes through the IR, never
// family-to-family. Unknown surfaces fall back to the chat codec —
// translation must stay total.

// ConversionQuality is the per-turn quality vocabulary: lossless (no
// diagnostics), lossy-but-documented (warnings only — every drop recorded),
// degraded (an error-severity loss occurred).
type ConversionQuality string

const (
	QualityLossless        ConversionQuality = "lossless"
	QualityLossyDocumented ConversionQuality = "lossy_documented"
	QualityDegraded        ConversionQuality = "degraded"
)

// QualityFor derives the turn quality from its diagnostics.
func QualityFor(diags []IRDiagnostic) ConversionQuality {
	quality := QualityLossless
	for _, d := range diags {
		if d.Severity == SeverityError {
			return QualityDegraded
		}
		quality = QualityLossyDocumented
	}
	return quality
}

// RequestDecoder is one surface's Decode leg: raw body → IRRequest.
type RequestDecoder func(body []byte) *IRRequest

var requestDecoders = map[SurfaceFormat]RequestDecoder{
	SurfaceChat:      DecodeRequestIR,
	SurfaceResponses: DecodeRequestIR,
	SurfaceAnthropic: DecodeRequestIR,
}

// RegisterRequestDecoder adds (or replaces) the decoder for a surface.
// Surfaces are onboarded as decoder + renderer pairs; never as pipeline
// surgery.
func RegisterRequestDecoder(surface SurfaceFormat, decode RequestDecoder) {
	requestDecoders[surface] = decode
}

// RequestDecoderFor returns the decoder for a surface, falling back to the
// chat codec for unknown surfaces (translation stays total).
func RequestDecoderFor(surface SurfaceFormat) RequestDecoder {
	if d, ok := requestDecoders[surface]; ok {
		return d
	}
	return requestDecoders[SurfaceChat]
}

// RegisteredSurfaces returns the surfaces with an explicit decoder.
func RegisteredSurfaces() []SurfaceFormat {
	out := make([]SurfaceFormat, 0, len(requestDecoders))
	for s := range requestDecoders {
		out = append(out, s)
	}
	return out
}

// ResponseRenderer is one surface's Render leg: IR calls → client calls.
// Implementations MUST delegate argument shaping to the family renderers
// (ir_render.go) so all three surfaces stay byte-identical per family.
type ResponseRenderer interface {
	// Surface is the client surface this renderer encodes for.
	Surface() SurfaceFormat
	// Render maps wire calls to client dispatches for one turn.
	Render(mapper ToolMapper, calls []IRCall) []RenderedCall
}

var responseRenderers = map[SurfaceFormat]ResponseRenderer{}

// RegisterResponseRenderer adds (or replaces) the renderer for a surface.
func RegisterResponseRenderer(renderer ResponseRenderer) {
	responseRenderers[renderer.Surface()] = renderer
}

// ResponseRendererFor returns the renderer for a surface, or nil when none
// is registered (the chat relay path is the default and needs no entry).
func ResponseRendererFor(surface SurfaceFormat) ResponseRenderer {
	return responseRenderers[surface]
}

// chatRenderer is the default response renderer: one []IRCall in, client
// dispatches out, via the shared family render path.
type chatRenderer struct{}

func (chatRenderer) Surface() SurfaceFormat { return SurfaceChat }

func (chatRenderer) Render(mapper ToolMapper, calls []IRCall) []RenderedCall {
	return RenderIRCalls(mapper, calls)
}

func init() {
	RegisterResponseRenderer(chatRenderer{})
}
