// Package anthropic implements the [models.Model] interface against the
// Anthropic Messages API using streaming SSE and tool use.
//
// # Design
//
// The adapter is implemented with stdlib net/http only (no Anthropic SDK) to
// keep dependencies minimal and to allow httptest servers in unit tests.
//
// # Streaming
//
// The adapter sends a single POST /v1/messages with "stream": true and parses
// the SSE response body line-by-line. Each SSE event is mapped to a
// [models.Event]; events with no normalized counterpart (e.g. thinking blocks,
// content_block_start for text) surface as [models.KindProviderEvent].
//
// # Tool use
//
// Tool input arrives as a sequence of input_json_delta fragments tied to a
// content block index. The adapter accumulates per-index buffers, emits
// [models.KindToolCallDelta] for each fragment, and emits
// [models.KindToolCallDone] at content_block_stop with the fully assembled
// [models.ToolCall].
//
// # Prompt caching
//
// The system prompt is sent with cache_control: {"type": "ephemeral"} on the
// system content block so Anthropic can cache it across turns.
//
// # Credentials
//
// The adapter receives an API key and a base URL; it mints no credentials and
// owns no egress policy. The trust-plane sidecar brokers the scoped endpoint
// and bearer token (spec: credentials/trust-plane-sidecar-lux.md).
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"latere.ai/x/agents/internal/models"
)

const (
	defaultBaseURL   = "https://api.anthropic.com"
	defaultModel     = "claude-opus-4-7"
	anthropicVersion = "2023-06-01"

	// anthropicBetaThinking is the beta header required to enable extended
	// thinking (interleaved thinking). It is set only when ThinkingBudget > 0.
	anthropicBetaThinking = "interleaved-thinking-2025-05-14"

	// anthropicBetaOAuth is the beta header the Anthropic API requires when
	// authenticating with an OAuth access token (Authorization: Bearer) rather
	// than an API key. Enabled by [WithOAuthToken].
	anthropicBetaOAuth = "oauth-2025-04-20"
)

// Option configures an [Adapter].
type Option func(*Adapter)

// WithModel overrides the default Anthropic model name.
func WithModel(model string) Option {
	return func(a *Adapter) { a.model = model }
}

// WithHTTPClient overrides the HTTP client used for requests.
func WithHTTPClient(c *http.Client) Option {
	return func(a *Adapter) { a.httpClient = c }
}

// WithOAuthToken configures the adapter to authenticate with an OAuth access
// token (e.g. CLAUDE_CODE_OAUTH_TOKEN, prefix "sk-ant-oat") instead of an API
// key. The credential passed to [New] is then sent as the
// "Authorization: Bearer <token>" header and the "oauth-2025-04-20" beta is
// enabled, both of which the Anthropic API requires for OAuth tokens — they are
// rejected on the x-api-key header. API keys (the default) ignore this.
func WithOAuthToken() Option {
	return func(a *Adapter) {
		a.useBearer = true
		a.betas = append(a.betas, anthropicBetaOAuth)
	}
}

// Adapter implements [models.Model] against the Anthropic Messages API.
// Create with [New]; the zero value is not usable.
type Adapter struct {
	apiKey     string
	baseURL    string
	model      string
	httpClient *http.Client
	// useBearer sends the credential as "Authorization: Bearer" instead of
	// "x-api-key" (set by [WithOAuthToken]).
	useBearer bool
	// betas are always-on anthropic-beta values (e.g. OAuth); per-request betas
	// such as thinking are merged in at request time.
	betas []string
}

// New returns an Adapter ready to call the Anthropic Messages API.
//
// apiKey is the Anthropic API key (x-api-key header). baseURL defaults to
// "https://api.anthropic.com" when empty; pass an httptest.Server URL in
// tests. opts apply optional overrides.
func New(apiKey, baseURL string, opts ...Option) *Adapter {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	a := &Adapter{
		apiKey:     apiKey,
		baseURL:    strings.TrimRight(baseURL, "/"),
		model:      defaultModel,
		httpClient: http.DefaultClient,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Model returns the model id the adapter will request (the default or the
// [WithModel] override). Exposed for wiring/observability.
func (a *Adapter) Model() string { return a.model }

// Stream implements [models.Model].
func (a *Adapter) Stream(ctx context.Context, req models.Request) (models.Stream, error) {
	body, err := a.buildRequest(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if a.useBearer {
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
	} else {
		httpReq.Header.Set("x-api-key", a.apiKey)
	}
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	httpReq.Header.Set("Accept", "text/event-stream")
	// Merge always-on betas (e.g. OAuth) with the per-request thinking beta,
	// which is enabled only when a budget is requested. Copy before appending so
	// a.betas is never mutated.
	betas := a.betas
	if req.ThinkingBudget > 0 {
		betas = append(append([]string(nil), betas...), anthropicBetaThinking)
	}
	if len(betas) > 0 {
		httpReq.Header.Set("anthropic-beta", strings.Join(betas, ","))
	}

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: http: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &APIError{Status: resp.StatusCode, Body: string(body)}
	}

	return newStream(ctx, resp.Body), nil
}

// APIError carries an HTTP-level error from the Anthropic API.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("anthropic: API error %d: %s", e.Status, e.Body)
}

// ---------------------------------------------------------------------------
// Wire types (Anthropic Messages API request shape)
// ---------------------------------------------------------------------------

type wireRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    []wireContent `json:"system,omitempty"`
	Messages  []wireMessage `json:"messages"`
	Tools     []wireTool    `json:"tools,omitempty"`
	Stream    bool          `json:"stream"`
	// Thinking budget: omit when zero.
	Thinking *wireThinking `json:"thinking,omitempty"`
	// Temperature: omit when zero (use provider default).
	Temperature *float64 `json:"temperature,omitempty"`
}

type wireThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type wireMessage struct {
	Role    string        `json:"role"`
	Content []wireContent `json:"content"`
}

type wireContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// For tool_use content blocks (assistant role).
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// For tool_result content blocks (user role).
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	// For cache_control on system and user content blocks.
	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
}

type wireCacheControl struct {
	Type string `json:"type"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ---------------------------------------------------------------------------
// Request builder
// ---------------------------------------------------------------------------

func (a *Adapter) buildRequest(req models.Request) ([]byte, error) {
	wr := wireRequest{
		Model:     a.model,
		MaxTokens: req.MaxTokens,
		Stream:    true,
	}
	if wr.MaxTokens == 0 {
		wr.MaxTokens = 8192 // safe default
	}

	// System block with prompt caching.
	if req.System != "" {
		wr.System = []wireContent{
			{
				Type:         "text",
				Text:         req.System,
				CacheControl: &wireCacheControl{Type: "ephemeral"},
			},
		}
	}

	// Temperature: only set if non-zero.
	if req.Temperature != 0 {
		t := req.Temperature
		wr.Temperature = &t
	}

	// Extended thinking.
	if req.ThinkingBudget > 0 {
		wr.Thinking = &wireThinking{
			Type:         "enabled",
			BudgetTokens: req.ThinkingBudget,
		}
	}

	// Tools.
	for _, td := range req.Tools {
		schema := td.InputSchema
		if schema == nil {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		wr.Tools = append(wr.Tools, wireTool{
			Name:        td.Name,
			Description: td.Description,
			InputSchema: schema,
		})
	}

	// Messages: map canonical roles to Anthropic wire shape.
	for _, m := range req.Messages {
		wm, err := messageToWire(m)
		if err != nil {
			return nil, err
		}
		wr.Messages = append(wr.Messages, wm)
	}

	return json.Marshal(wr)
}

// messageToWire converts a canonical [models.Message] to the Anthropic wire
// message shape.
//
// Key mapping:
//
//   - "user" → role "user", type "text"
//   - "assistant" → role "assistant"; text content + tool_use blocks
//   - "tool" → role "user" with tool_result content blocks (Anthropic does
//     not have a "tool" role; results are wrapped in user messages)
func messageToWire(m models.Message) (wireMessage, error) {
	switch m.Role {
	case "user":
		return wireMessage{
			Role:    "user",
			Content: []wireContent{{Type: "text", Text: m.Content}},
		}, nil

	case "assistant":
		var contents []wireContent
		if m.Content != "" {
			contents = append(contents, wireContent{Type: "text", Text: m.Content})
		}
		for _, tc := range m.ToolCalls {
			input := tc.Input
			if input == nil {
				input = json.RawMessage(`{}`)
			}
			contents = append(contents, wireContent{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Name,
				Input: input,
			})
		}
		if len(contents) == 0 {
			contents = []wireContent{{Type: "text", Text: ""}}
		}
		return wireMessage{Role: "assistant", Content: contents}, nil

	case "tool":
		// Anthropic encodes tool results as a "user" message with
		// "tool_result" content blocks, one per ToolResult.
		var contents []wireContent
		for _, tr := range m.ToolResults {
			contents = append(contents, wireContent{
				Type:      "tool_result",
				ToolUseID: tr.CallID,
				Content:   tr.Content,
				IsError:   tr.IsError,
			})
		}
		if len(contents) == 0 {
			return wireMessage{}, fmt.Errorf("anthropic: tool message has no ToolResults")
		}
		return wireMessage{Role: "user", Content: contents}, nil

	default:
		return wireMessage{}, fmt.Errorf("anthropic: unknown message role %q", m.Role)
	}
}

// ---------------------------------------------------------------------------
// SSE stream
// ---------------------------------------------------------------------------

// stream implements [models.Stream] by reading SSE events from an HTTP
// response body.
type stream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	done    bool
	mu      sync.Mutex

	// Per content-block state: index → accumulated tool-call info.
	toolBlocks map[int]*toolBlock

	// pendingStopReason is set by message_delta and consumed by message_stop.
	pendingStopReason models.StopReason
}

type toolBlock struct {
	id    string
	name  string
	input strings.Builder
}

func newStream(_ context.Context, body io.ReadCloser) *stream {
	sc := bufio.NewScanner(body)
	// Anthropic tool inputs and thinking blocks can produce long SSE lines.
	// Raise the Scanner buffer from the default 64 KiB to 1 MiB to avoid
	// ErrTooLong on large payloads.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return &stream{
		body:       body,
		scanner:    sc,
		toolBlocks: make(map[int]*toolBlock),
	}
}

// Recv implements [models.Stream]. It returns events one at a time. After
// the KindDone event, the next call returns io.EOF.
func (s *stream) Recv() (models.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.done {
		return models.Event{}, io.EOF
	}

	for {
		ev, data, ok, err := s.readSSEEvent()
		if err != nil {
			return models.Event{}, err
		}
		if !ok {
			// Scanner returned false without data — EOF from server.
			s.done = true
			return models.Event{}, io.EOF
		}
		if ev == "" || data == "" || data == "[DONE]" {
			continue
		}

		event, skip, fatal := s.mapEvent(ev, []byte(data))
		if fatal != nil {
			return models.Event{}, fatal
		}
		if skip {
			continue
		}
		if event.Kind == models.KindDone {
			s.done = true
		}
		return event, nil
	}
}

// readSSEEvent reads lines until a complete event (event: / data: pair) is
// accumulated, then returns the event type and data. Returns (_, _, false, nil)
// on clean EOF.
func (s *stream) readSSEEvent() (eventType, data string, ok bool, err error) {
	for {
		if !s.scanner.Scan() {
			if scanErr := s.scanner.Err(); scanErr != nil {
				return "", "", false, fmt.Errorf("anthropic: stream read: %w", scanErr)
			}
			return "", "", false, nil // clean EOF
		}
		line := s.scanner.Text()

		switch {
		case strings.HasPrefix(line, "event: "):
			eventType = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "":
			// Blank line: end of event. Return if we have both pieces.
			if eventType != "" && data != "" {
				return eventType, data, true, nil
			}
			// Reset partial state and keep reading.
			eventType = ""
			data = ""
		}
	}
}

// ---------------------------------------------------------------------------
// SSE wire types (Anthropic response events)
// ---------------------------------------------------------------------------

type sseMessageStart struct {
	Type    string     `json:"type"`
	Message sseMessage `json:"message"`
}

type sseMessage struct {
	Usage sseUsage `json:"usage"`
}

type sseUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type sseContentBlockStart struct {
	Type         string          `json:"type"`
	Index        int             `json:"index"`
	ContentBlock sseContentBlock `json:"content_block"`
}

type sseContentBlock struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
	Text string `json:"text"`
}

type sseContentBlockDelta struct {
	Type  string   `json:"type"`
	Index int      `json:"index"`
	Delta sseDelta `json:"delta"`
}

type sseDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
	// For thinking blocks.
	Thinking string `json:"thinking"`
}

type sseContentBlockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type sseMessageDelta struct {
	Type  string       `json:"type"`
	Delta sseStopDelta `json:"delta"`
	Usage sseUsage     `json:"usage"`
}

type sseStopDelta struct {
	StopReason   string `json:"stop_reason"`
	StopSequence string `json:"stop_sequence"`
}

// mapEvent converts a single SSE event into a normalized [models.Event].
// Returns (event, skip=true, nil) for events that should be silently skipped
// (e.g. ping, content_block_start for text). Returns (event, false, nil) on
// success.
func (s *stream) mapEvent(eventType string, data []byte) (ev models.Event, skip bool, err error) {
	switch eventType {
	case "message_start":
		var ms sseMessageStart
		if err := json.Unmarshal(data, &ms); err != nil {
			return models.Event{}, false, fmt.Errorf("anthropic: parse message_start: %w", err)
		}
		return models.Event{
			Kind: models.KindUsage,
			Usage: &models.Usage{
				InputTokens:      ms.Message.Usage.InputTokens,
				OutputTokens:     ms.Message.Usage.OutputTokens,
				CacheReadTokens:  ms.Message.Usage.CacheReadInputTokens,
				CacheWriteTokens: ms.Message.Usage.CacheCreationInputTokens,
			},
		}, false, nil

	case "content_block_start":
		var cbs sseContentBlockStart
		if err := json.Unmarshal(data, &cbs); err != nil {
			return models.Event{}, false, fmt.Errorf("anthropic: parse content_block_start: %w", err)
		}
		switch cbs.ContentBlock.Type {
		case "tool_use":
			// Register the tool block so deltas can be accumulated.
			s.toolBlocks[cbs.Index] = &toolBlock{
				id:   cbs.ContentBlock.ID,
				name: cbs.ContentBlock.Name,
			}
			// Surface as a ProviderEvent (observational only).
			return models.Event{
				Kind: models.KindProviderEvent,
				ProviderEvent: &models.ProviderEvent{
					Type: "content_block_start.tool_use",
					Raw:  data,
				},
			}, false, nil
		case "thinking":
			return models.Event{
				Kind: models.KindProviderEvent,
				ProviderEvent: &models.ProviderEvent{
					Type: "content_block_start.thinking",
					Raw:  data,
				},
			}, false, nil
		default:
			// text and others: skip (no normalized counterpart at start).
			return models.Event{}, true, nil
		}

	case "content_block_delta":
		var cbd sseContentBlockDelta
		if err := json.Unmarshal(data, &cbd); err != nil {
			return models.Event{}, false, fmt.Errorf("anthropic: parse content_block_delta: %w", err)
		}
		switch cbd.Delta.Type {
		case "text_delta":
			return models.Event{
				Kind:      models.KindTextDelta,
				TextDelta: cbd.Delta.Text,
			}, false, nil

		case "input_json_delta":
			frag := cbd.Delta.PartialJSON
			if tb, ok := s.toolBlocks[cbd.Index]; ok {
				tb.input.WriteString(frag)
			}
			return models.Event{
				Kind:          models.KindToolCallDelta,
				ToolCallIndex: cbd.Index,
				ToolCallDelta: frag,
			}, false, nil

		case "thinking_delta":
			// Thinking content: surface as ProviderEvent.
			return models.Event{
				Kind: models.KindProviderEvent,
				ProviderEvent: &models.ProviderEvent{
					Type: "thinking_delta",
					Raw:  data,
				},
			}, false, nil

		case "signature_delta":
			// Anthropic extended thinking signature: observational.
			return models.Event{
				Kind: models.KindProviderEvent,
				ProviderEvent: &models.ProviderEvent{
					Type: "signature_delta",
					Raw:  data,
				},
			}, false, nil

		default:
			// Unknown delta type: pass through.
			return models.Event{
				Kind: models.KindProviderEvent,
				ProviderEvent: &models.ProviderEvent{
					Type: "content_block_delta." + cbd.Delta.Type,
					Raw:  data,
				},
			}, false, nil
		}

	case "content_block_stop":
		var cbs sseContentBlockStop
		if err := json.Unmarshal(data, &cbs); err != nil {
			return models.Event{}, false, fmt.Errorf("anthropic: parse content_block_stop: %w", err)
		}
		tb, found := s.toolBlocks[cbs.Index]
		if !found {
			// Text block stop — no normalized event.
			return models.Event{}, true, nil
		}
		assembled := tb.input.String()
		if assembled == "" {
			assembled = "{}"
		}
		tc := &models.ToolCall{
			ID:    tb.id,
			Name:  tb.name,
			Input: json.RawMessage(assembled),
		}
		delete(s.toolBlocks, cbs.Index)
		return models.Event{
			Kind:     models.KindToolCallDone,
			ToolCall: tc,
		}, false, nil

	case "message_delta":
		var md sseMessageDelta
		if err := json.Unmarshal(data, &md); err != nil {
			return models.Event{}, false, fmt.Errorf("anthropic: parse message_delta: %w", err)
		}
		// Emit usage update first; Done will be emitted on message_stop.
		// However, spec says Done carries StopReason, and message_stop carries
		// no data — so we emit Done here with the usage bundled.
		//
		// Implementation note: we emit KindUsage here and return KindDone
		// on message_stop. But message_delta carries both stop_reason and
		// usage; we store the stop reason and emit it on message_stop.
		//
		// To avoid two Recv calls for one logical "done", we emit the usage
		// event immediately and stash the stop reason for the message_stop.
		s.pendingStopReason = mapStopReason(md.Delta.StopReason)
		return models.Event{
			Kind: models.KindUsage,
			Usage: &models.Usage{
				InputTokens:      md.Usage.InputTokens,
				OutputTokens:     md.Usage.OutputTokens,
				CacheReadTokens:  md.Usage.CacheReadInputTokens,
				CacheWriteTokens: md.Usage.CacheCreationInputTokens,
			},
		}, false, nil

	case "message_stop":
		return models.Event{
			Kind:       models.KindDone,
			StopReason: s.pendingStopReason,
		}, false, nil

	case "ping":
		return models.Event{}, true, nil

	case "error":
		return models.Event{}, false, fmt.Errorf("anthropic: stream error event: %s", data)

	default:
		// Unknown event: pass through as ProviderEvent.
		return models.Event{
			Kind: models.KindProviderEvent,
			ProviderEvent: &models.ProviderEvent{
				Type: eventType,
				Raw:  data,
			},
		}, false, nil
	}
}

// Close implements [models.Stream].
func (s *stream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.Close()
}

// mapStopReason converts an Anthropic stop_reason string to a canonical
// [models.StopReason].
func mapStopReason(reason string) models.StopReason {
	switch reason {
	case "end_turn":
		return models.StopEndTurn
	case "tool_use":
		return models.StopToolUse
	case "max_tokens":
		return models.StopMaxTokens
	case "stop_sequence":
		return models.StopSequence
	default:
		return models.StopReason(reason)
	}
}

// Ensure Adapter implements models.Model at compile time.
var _ models.Model = (*Adapter)(nil)
