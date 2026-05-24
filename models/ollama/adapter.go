// Package ollama implements the [models.Model] interface against the Ollama
// local inference server using the /api/chat streaming endpoint.
//
// # Design
//
// The adapter is implemented with stdlib net/http only to keep dependencies
// minimal and to allow httptest servers in unit tests.
//
// # Streaming
//
// The adapter sends a single POST /api/chat with "stream": true and parses the
// newline-delimited JSON (NDJSON) response body using json.Decoder. Each JSON
// object (chunk) is mapped to zero or more normalized [models.Event]s; the
// internal event queue drains one event per Recv call so callers see a clean
// one-event-at-a-time interface.
//
// # Tool use
//
// Ollama delivers tool arguments as a JSON object in one atomic chunk (not
// incrementally). The adapter emits a single [models.KindToolCallDone] per tool
// call (no deltas). Call IDs are synthesized as "call_<n>" because Ollama does
// not assign IDs.
//
// # Stop reason
//
// The final chunk carries done:true and done_reason. The adapter tracks whether
// any tool_calls were seen across the stream; if so, StopReason is StopToolUse
// regardless of done_reason.
//
// # Credentials
//
// The adapter receives a host URL; it mints no credentials and owns no egress
// policy.
package ollama

import (
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
	defaultHost  = "http://localhost:11434"
	defaultModel = "llama3.1"
)

// Adapter implements [models.Model] against the Ollama /api/chat endpoint.
// Create with [New]; the zero value is not usable.
type Adapter struct {
	host       string
	model      string
	httpClient *http.Client
}

// New returns an Adapter ready to call the Ollama /api/chat endpoint.
//
// host is the Ollama base URL (e.g. "http://localhost:11434"). When empty,
// it defaults to "http://localhost:11434". modelName is the Ollama model tag
// (e.g. "llama3.1", "qwen2.5:0.5b").
func New(host, modelName string) *Adapter {
	if host == "" {
		host = defaultHost
	}
	if modelName == "" {
		modelName = defaultModel
	}
	return &Adapter{
		host:       strings.TrimRight(host, "/"),
		model:      modelName,
		httpClient: http.DefaultClient,
	}
}

// Stream implements [models.Model].
func (a *Adapter) Stream(ctx context.Context, req models.Request) (models.Stream, error) {
	body, err := a.buildRequest(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: build request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.host+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama: http: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &APIError{Status: resp.StatusCode, Body: string(errBody)}
	}

	return newStream(resp.Body), nil
}

// APIError carries an HTTP-level error from the Ollama server.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ollama: API error %d: %s", e.Status, e.Body)
}

// ---------------------------------------------------------------------------
// Wire types (Ollama /api/chat request shape)
// ---------------------------------------------------------------------------

type wireRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Tools    []wireTool    `json:"tools,omitempty"`
	Stream   bool          `json:"stream"`
	Options  *wireOptions  `json:"options,omitempty"`
}

type wireMessage struct {
	Role      string                  `json:"role"`
	Content   string                  `json:"content"`
	ToolCalls []wireAssistantToolCall `json:"tool_calls,omitempty"`
}

// wireAssistantToolCall encodes a prior tool call on an assistant history
// message, so the model can correlate tool results in multi-turn flows.
type wireAssistantToolCall struct {
	Function wireAssistantToolCallFunction `json:"function"`
}

type wireAssistantToolCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type wireTool struct {
	Type     string           `json:"type"`
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireOptions struct {
	// Temperature controls randomness. Omitted when zero.
	Temperature *float64 `json:"temperature,omitempty"`
	// NumPredict caps generated tokens. Omitted when zero.
	NumPredict *int `json:"num_predict,omitempty"`
}

// ---------------------------------------------------------------------------
// Request builder
// ---------------------------------------------------------------------------

func (a *Adapter) buildRequest(req models.Request) ([]byte, error) {
	wr := wireRequest{
		Model:  a.model,
		Stream: true,
	}

	// System prompt as a leading system message.
	if req.System != "" {
		wr.Messages = append(wr.Messages, wireMessage{
			Role:    "system",
			Content: req.System,
		})
	}

	// Convert canonical messages to Ollama wire messages.
	for _, m := range req.Messages {
		wm, err := messageToWire(m)
		if err != nil {
			return nil, err
		}
		wr.Messages = append(wr.Messages, wm...)
	}

	// Tools.
	for _, td := range req.Tools {
		schema := td.InputSchema
		if schema == nil {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		wr.Tools = append(wr.Tools, wireTool{
			Type: "function",
			Function: wireToolFunction{
				Name:        td.Name,
				Description: td.Description,
				Parameters:  schema,
			},
		})
	}

	// Options: set only non-zero values.
	var opts wireOptions
	hasOpts := false
	if req.Temperature != 0 {
		t := req.Temperature
		opts.Temperature = &t
		hasOpts = true
	}
	if req.MaxTokens > 0 {
		n := req.MaxTokens
		opts.NumPredict = &n
		hasOpts = true
	}
	if hasOpts {
		wr.Options = &opts
	}

	return json.Marshal(wr)
}

// messageToWire converts a canonical [models.Message] to one or more Ollama
// wire messages.
//
// Key mapping:
//
//   - "user" → role "user", content as plain string
//   - "assistant" → role "assistant"; content as plain string, with any
//     ToolCalls serialized into the Ollama tool_calls array so the model
//     can correlate tool results in multi-turn flows.
//   - "tool" → one "tool"-role message per ToolResult
func messageToWire(m models.Message) ([]wireMessage, error) {
	switch m.Role {
	case "user":
		return []wireMessage{{Role: "user", Content: m.Content}}, nil

	case "assistant":
		wm := wireMessage{Role: "assistant", Content: m.Content}
		for _, tc := range m.ToolCalls {
			input := tc.Input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			wm.ToolCalls = append(wm.ToolCalls, wireAssistantToolCall{
				Function: wireAssistantToolCallFunction{
					Name:      tc.Name,
					Arguments: input,
				},
			})
		}
		return []wireMessage{wm}, nil

	case "tool":
		if len(m.ToolResults) == 0 {
			return nil, fmt.Errorf("ollama: tool message has no ToolResults")
		}
		var msgs []wireMessage
		for _, tr := range m.ToolResults {
			msgs = append(msgs, wireMessage{
				Role:    "tool",
				Content: tr.Content,
			})
		}
		return msgs, nil

	default:
		return nil, fmt.Errorf("ollama: unknown message role %q", m.Role)
	}
}

// ---------------------------------------------------------------------------
// NDJSON stream wire types (Ollama response chunk shape)
// ---------------------------------------------------------------------------

type wireChunk struct {
	Message         *wireChunkMessage `json:"message"`
	Done            bool              `json:"done"`
	DoneReason      string            `json:"done_reason"`
	PromptEvalCount int               `json:"prompt_eval_count"`
	EvalCount       int               `json:"eval_count"`
}

type wireChunkMessage struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolCalls []wireToolCall `json:"tool_calls"`
}

type wireToolCall struct {
	Function wireToolCallFunction `json:"function"`
}

type wireToolCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ---------------------------------------------------------------------------
// Stream implementation
// ---------------------------------------------------------------------------

// stream implements [models.Stream] by reading NDJSON chunks from an Ollama
// HTTP response body.
type stream struct {
	body    io.ReadCloser
	decoder *json.Decoder
	done    bool
	mu      sync.Mutex

	// queue holds events decoded from the current chunk but not yet delivered.
	queue []models.Event

	// callIndex is used to synthesize unique tool-call IDs.
	callIndex int

	// sawToolCall tracks whether any tool call was seen (used for stop-reason).
	sawToolCall bool
}

func newStream(body io.ReadCloser) *stream {
	return &stream{
		body:    body,
		decoder: json.NewDecoder(body),
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
		// Drain the queue first.
		if len(s.queue) > 0 {
			ev := s.queue[0]
			s.queue = s.queue[1:]
			if ev.Kind == models.KindDone {
				s.done = true
			}
			return ev, nil
		}

		// Decode the next NDJSON chunk.
		var chunk wireChunk
		if err := s.decoder.Decode(&chunk); err != nil {
			if err == io.EOF {
				s.done = true
				return models.Event{}, io.EOF
			}
			return models.Event{}, fmt.Errorf("ollama: decode chunk: %w", err)
		}

		s.enqueueChunk(chunk)
	}
}

// enqueueChunk converts one NDJSON chunk into normalized events and appends
// them to the queue.
func (s *stream) enqueueChunk(chunk wireChunk) {
	if chunk.Message != nil {
		// Text delta.
		if chunk.Message.Content != "" {
			s.queue = append(s.queue, models.Event{
				Kind:      models.KindTextDelta,
				TextDelta: chunk.Message.Content,
			})
		}

		// Tool calls: Ollama delivers full argument objects atomically.
		for _, tc := range chunk.Message.ToolCalls {
			s.sawToolCall = true
			s.callIndex++
			id := fmt.Sprintf("call_%d", s.callIndex)

			input := tc.Function.Arguments
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}

			s.queue = append(s.queue, models.Event{
				Kind: models.KindToolCallDone,
				ToolCall: &models.ToolCall{
					ID:    id,
					Name:  tc.Function.Name,
					Input: input,
				},
			})
		}
	}

	// On the terminal chunk, emit Usage then Done.
	if chunk.Done {
		s.queue = append(s.queue, models.Event{
			Kind: models.KindUsage,
			Usage: &models.Usage{
				InputTokens:  chunk.PromptEvalCount,
				OutputTokens: chunk.EvalCount,
			},
		})

		s.queue = append(s.queue, models.Event{
			Kind:       models.KindDone,
			StopReason: s.mapStopReason(chunk.DoneReason),
		})
	}
}

// mapStopReason converts an Ollama done_reason string to a canonical
// [models.StopReason]. The sawToolCall flag takes precedence.
func (s *stream) mapStopReason(doneReason string) models.StopReason {
	if s.sawToolCall {
		return models.StopToolUse
	}
	switch doneReason {
	case "length":
		return models.StopMaxTokens
	case "stop", "":
		return models.StopEndTurn
	default:
		return models.StopReason(doneReason)
	}
}

// Close implements [models.Stream].
func (s *stream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.Close()
}

// Ensure Adapter implements models.Model at compile time.
var _ models.Model = (*Adapter)(nil)
