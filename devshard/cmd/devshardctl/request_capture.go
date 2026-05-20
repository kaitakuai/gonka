package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"devshard/user"
)

const requestCaptureDirName = "captured-requests"

type requestCaptureStore struct {
	dir string
}

type capturedChatRequest struct {
	RequestID  string          `json:"request_id,omitempty"`
	CapturedAt string          `json:"captured_at"`
	Kind       string          `json:"kind"`
	Error      string          `json:"error,omitempty"`
	Method     string          `json:"method,omitempty"`
	Path       string          `json:"path,omitempty"`
	Model      string          `json:"model,omitempty"`
	Escrow     string          `json:"escrow,omitempty"`
	Stream     bool            `json:"stream,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
	BodyBase64 string          `json:"body_base64,omitempty"`
}

var (
	requestCaptureMu     sync.RWMutex
	activeRequestCapture *requestCaptureStore
)

func configureRequestCaptureStore(baseStorageDir string) {
	dir := strings.TrimSpace(os.Getenv("DEVSHARD_REQUEST_CAPTURE_DIR"))
	if dir == "" && readBoolEnv("DEVSHARD_REQUEST_CAPTURE_DISABLED", false) {
		setRequestCaptureStore(nil)
		return
	}
	if dir == "" {
		dir = filepath.Join(baseStorageDir, requestCaptureDirName)
	}
	setRequestCaptureStore(&requestCaptureStore{dir: dir})
}

func setRequestCaptureStore(store *requestCaptureStore) {
	requestCaptureMu.Lock()
	defer requestCaptureMu.Unlock()
	activeRequestCapture = store
}

func currentRequestCaptureStore() *requestCaptureStore {
	requestCaptureMu.RLock()
	defer requestCaptureMu.RUnlock()
	return activeRequestCapture
}

func captureFilterRejectedRequest(r *http.Request, body []byte, err error, model, escrow string) {
	if r == nil || err == nil || len(body) == 0 {
		return
	}
	store := currentRequestCaptureStore()
	if store == nil {
		return
	}
	record := capturedChatRequest{
		RequestID:  requestIDOrEmpty(r.Context()),
		CapturedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Kind:       "filter_rejected",
		Error:      err.Error(),
		Method:     r.Method,
		Path:       requestPath(r),
		Model:      firstNonEmpty(model, chatRequestModel(body)),
		Escrow:     escrow,
		Stream:     chatRequestStream(body),
	}
	setCapturedRequestBody(&record, body)
	_ = store.write(record)
}

func captureAllAttemptsFailedRequest(ctx context.Context, escrow string, params user.InferenceParams, err error) {
	if len(params.Prompt) == 0 {
		return
	}
	store := currentRequestCaptureStore()
	if store == nil {
		return
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	record := capturedChatRequest{
		RequestID:  requestIDOrEmpty(ctx),
		CapturedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Kind:       "all_attempts_failed",
		Error:      errText,
		Path:       "/v1/chat/completions",
		Model:      params.Model,
		Escrow:     escrow,
		Stream:     params.Stream,
	}
	setCapturedRequestBody(&record, params.Prompt)
	_ = store.write(record)
}

func (s *requestCaptureStore) write(record capturedChatRequest) error {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return nil
	}
	kind := safeFilenameComponent(record.Kind)
	if kind == "" {
		kind = "request"
	}
	dir := filepath.Join(s.dir, kind)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')

	name := fmt.Sprintf("%s_%s_%s.json",
		time.Now().UTC().Format("20060102T150405.000000000Z"),
		safeFilenameComponent(firstNonEmpty(record.RequestID, "no-request-id")),
		kind,
	)
	path := filepath.Join(dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func setCapturedRequestBody(record *capturedChatRequest, body []byte) {
	if record == nil || len(body) == 0 {
		return
	}
	if json.Valid(body) {
		record.Body = append(json.RawMessage(nil), body...)
		return
	}
	record.BodyBase64 = base64.StdEncoding.EncodeToString(body)
}

func requestIDOrEmpty(ctx context.Context) string {
	if requestID, ok := requestLogFromContext(ctx); ok {
		return requestID
	}
	return ""
}

func requestPath(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	return r.URL.Path
}

func chatRequestStream(body []byte) bool {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}
	return req.Stream
}

func safeFilenameComponent(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "._")
}
