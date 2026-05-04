package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type GatewayDisabledSettings struct {
	Enabled bool   `json:"enabled"`
	Message string `json:"message,omitempty"`
}

const defaultGatewayDisabledMessage = "please use ... base url"

func (s GatewayDisabledSettings) WithDefaults() GatewayDisabledSettings {
	s.Message = strings.TrimSpace(s.Message)
	if s.Message == "" {
		s.Message = defaultGatewayDisabledMessage
	}
	return s
}

func (g *Gateway) disabledMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		disabled := g.settings.Disabled
		defaultModel := g.settings.DefaultModel
		g.mu.Unlock()
		disabled = disabled.WithDefaults()
		if !disabled.Enabled {
			next.ServeHTTP(w, r)
			return
		}
		if isAdminPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		req := readDisabledChatRequest(r)
		model := firstNonEmpty(req.Model, defaultModel)
		if req.Stream {
			writeDisabledChatStream(w, model, disabled.Message)
			return
		}
		writeDisabledChatCompletion(w, model, disabled.Message)
	})
}

func readDisabledChatRequest(r *http.Request) chatRequest {
	if r == nil || r.Body == nil {
		return chatRequest{}
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxChatRequestBodySize+1))
	if err != nil || len(body) == 0 || len(body) > MaxChatRequestBodySize {
		return chatRequest{}
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return chatRequest{}
	}
	return req
}

func writeDisabledChatCompletion(w http.ResponseWriter, model, message string) {
	now := time.Now().Unix()
	if model == "" {
		model = defaultModelName
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":      fmt.Sprintf("chatcmpl-gateway-disabled-%d", now),
		"object":  "chat.completion",
		"created": now,
		"model":   model,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": message,
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": len(strings.Fields(message)),
			"total_tokens":      len(strings.Fields(message)),
		},
	})
}

func writeDisabledChatStream(w http.ResponseWriter, model, message string) {
	now := time.Now().Unix()
	if model == "" {
		model = defaultModelName
	}
	id := fmt.Sprintf("chatcmpl-gateway-disabled-%d", now)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	writeSSEJSON := func(v any) {
		data, err := json.Marshal(v)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	}
	writeSSEJSON(map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": now,
		"model":   model,
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{
				"role":    "assistant",
				"content": message,
			},
			"finish_reason": nil,
		}},
	})
	writeSSEJSON(map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": now,
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
	})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
