package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/briankim06/adaptive-batching-engine/internal/batcher"
	"github.com/briankim06/adaptive-batching-engine/internal/models"
	"github.com/briankim06/adaptive-batching-engine/internal/worker"
)

// Types mirroring the OpenAI-compatible wire format documented in
// docs/spec/05-api.md.
type CompletionRequest struct {
	Prompt    string `json:"prompt"`
	MaxTokens int    `json:"max_tokens"`
	Priority  string `json:"priority"`
}

type CompletionChoice struct {
	Text  string `json:"text"`
	Index int    `json:"index"`
}

type CompletionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type CompletionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Choices []CompletionChoice `json:"choices"`
	Usage   CompletionUsage    `json:"usage"`
}

type EmbeddingRequest struct {
	Input    string `json:"input"`
	Priority string `json:"priority"`
}

type EmbeddingObject struct {
	Object    string    `json:"object"`
	Embedding []float64 `json:"embedding"`
	Index     int       `json:"index"`
}

type EmbeddingResponse struct {
	Object string            `json:"object"`
	Data   []EmbeddingObject `json:"data"`
}

type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// safetyMargin is subtracted from server.write_timeout so the handler has
// time to write a 504 body before the outer HTTP server kills the conn.
const safetyMargin = time.Second

// HandleCompletions returns a handler for POST /v1/completions.
func HandleCompletions(b batcher.Batcher, pool *worker.WorkerPool, writeTimeout time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
			return
		}
		if strings.TrimSpace(req.Prompt) == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "prompt is required")
			return
		}
		if req.MaxTokens <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "max_tokens must be > 0")
			return
		}

		if pool != nil && pool.AllWorkersUnhealthy() {
			writeError(w, http.StatusServiceUnavailable, "capacity", "all workers unavailable")
			return
		}

		ctx, cancel := deriveHandlerCtx(r.Context(), writeTimeout)
		defer cancel()

		priority := parsePriority(req.Priority)
		infReq := models.NewInferenceRequest(ctx, req.Prompt, req.MaxTokens, priority, models.RequestTypeCompletion)

		if err := b.Submit(ctx, infReq); err != nil {
			handleSubmitError(w, err)
			return
		}

		select {
		case result := <-infReq.ResultChan:
			if result == nil {
				writeError(w, http.StatusInternalServerError, "internal", "empty result")
				return
			}
			if result.Error != nil {
				if errors.Is(result.Error, models.ErrShuttingDown) {
					writeError(w, http.StatusServiceUnavailable, "capacity", result.Error.Error())
					return
				}
				writeError(w, http.StatusInternalServerError, "internal", result.Error.Error())
				return
			}
			text, completionTokens := extractCompletionText(result.Result)
			resp := CompletionResponse{
				ID:      "cmpl-" + infReq.ID,
				Object:  "text_completion",
				Created: time.Now().Unix(),
				Choices: []CompletionChoice{{Text: text, Index: 0}},
				Usage: CompletionUsage{
					PromptTokens:     estimatePromptTokens(req.Prompt),
					CompletionTokens: completionTokens,
					TotalTokens:      estimatePromptTokens(req.Prompt) + completionTokens,
				},
			}
			writeJSON(w, http.StatusOK, resp)

		case <-ctx.Done():
			writeError(w, http.StatusGatewayTimeout, "timeout", "request timed out")
			return
		}
	}
}

// HandleEmbeddings returns a handler for POST /v1/embeddings.
func HandleEmbeddings(b batcher.Batcher, pool *worker.WorkerPool, writeTimeout time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req EmbeddingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
			return
		}
		if strings.TrimSpace(req.Input) == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "input is required")
			return
		}

		if pool != nil && pool.AllWorkersUnhealthy() {
			writeError(w, http.StatusServiceUnavailable, "capacity", "all workers unavailable")
			return
		}

		ctx, cancel := deriveHandlerCtx(r.Context(), writeTimeout)
		defer cancel()

		priority := parsePriority(req.Priority)
		infReq := models.NewInferenceRequest(ctx, req.Input, 0, priority, models.RequestTypeEmbedding)

		if err := b.Submit(ctx, infReq); err != nil {
			handleSubmitError(w, err)
			return
		}

		select {
		case result := <-infReq.ResultChan:
			if result == nil {
				writeError(w, http.StatusInternalServerError, "internal", "empty result")
				return
			}
			if result.Error != nil {
				if errors.Is(result.Error, models.ErrShuttingDown) {
					writeError(w, http.StatusServiceUnavailable, "capacity", result.Error.Error())
					return
				}
				writeError(w, http.StatusInternalServerError, "internal", result.Error.Error())
				return
			}
			embedding := extractEmbedding(result.Result)
			resp := EmbeddingResponse{
				Object: "list",
				Data: []EmbeddingObject{{
					Object:    "embedding",
					Embedding: embedding,
					Index:     0,
				}},
			}
			writeJSON(w, http.StatusOK, resp)

		case <-ctx.Done():
			writeError(w, http.StatusGatewayTimeout, "timeout", "request timed out")
			return
		}
	}
}

// deriveHandlerCtx narrows the request context with writeTimeout - 1s so
// the handler can still write a 504 body before the outer HTTP server
// kills the connection. Falls back to r.Context() when writeTimeout is
// too small.
func deriveHandlerCtx(parent context.Context, writeTimeout time.Duration) (context.Context, context.CancelFunc) {
	budget := writeTimeout - safetyMargin
	if budget <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, budget)
}

func handleSubmitError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, models.ErrQueueFull):
		writeError(w, http.StatusServiceUnavailable, "capacity", "request queue is full")
	case errors.Is(err, models.ErrShuttingDown):
		writeError(w, http.StatusServiceUnavailable, "capacity", "server is shutting down")
	case errors.Is(err, models.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		writeError(w, http.StatusGatewayTimeout, "timeout", "request timed out")
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// parsePriority maps the wire string onto a Priority. Unknown or empty
// strings fall back to PriorityNormal.
func parsePriority(s string) models.Priority {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical":
		return models.PriorityCritical
	case "high":
		return models.PriorityHigh
	case "low":
		return models.PriorityLow
	default:
		return models.PriorityNormal
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, errType, msg string) {
	writeJSON(w, status, ErrorResponse{Error: ErrorBody{Message: msg, Type: errType}})
}

// estimatePromptTokens matches the token estimator used by
// InferenceRequest: roughly 4 characters per token.
func estimatePromptTokens(prompt string) int {
	n := len(prompt) / 4
	if n == 0 && len(prompt) > 0 {
		n = 1
	}
	return n
}

// extractCompletionText pulls a text string out of whatever the upstream
// sent back as the per-request payload. Falls back to a Go %v rendering
// if the payload is an unexpected shape so the caller still sees
// something useful.
func extractCompletionText(payload any) (string, int) {
	switch v := payload.(type) {
	case nil:
		return "", 0
	case string:
		return v, estimatePromptTokens(v)
	case map[string]any:
		if text, ok := v["text"].(string); ok {
			tokens := 0
			if f, ok := v["completion_tokens"].(float64); ok {
				tokens = int(f)
			} else {
				tokens = estimatePromptTokens(text)
			}
			return text, tokens
		}
	}
	s := fmt.Sprintf("%v", payload)
	return s, estimatePromptTokens(s)
}

func extractEmbedding(payload any) []float64 {
	switch v := payload.(type) {
	case nil:
		return []float64{}
	case []float64:
		return v
	case []any:
		out := make([]float64, 0, len(v))
		for _, e := range v {
			if f, ok := e.(float64); ok {
				out = append(out, f)
			}
		}
		return out
	case map[string]any:
		if emb, ok := v["embedding"].([]any); ok {
			out := make([]float64, 0, len(emb))
			for _, e := range emb {
				if f, ok := e.(float64); ok {
					out = append(out, f)
				}
			}
			return out
		}
	}
	return []float64{}
}
