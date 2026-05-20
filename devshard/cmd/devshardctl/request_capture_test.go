package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"devshard/user"

	"github.com/stretchr/testify/require"
)

func TestPrepareChatRequestBodyCapturesFilterRejectedRequest(t *testing.T) {
	captureDir := t.TempDir()
	setRequestCaptureStore(&requestCaptureStore{dir: captureDir})
	t.Cleanup(func() { setRequestCaptureStore(nil) })

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model": "Qwen/Test",
		"temperature": 0.7,
		"unsupported_field": true,
		"messages": [{"role": "user", "content": "hello"}]
	}`))
	ctx, requestID := ensureRequestLogContext(req.Context())
	req = req.WithContext(ctx)

	_, _, err := prepareChatRequestBody(req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported_field")

	record := requireSingleCapturedRequest(t, captureDir, "filter_rejected")
	require.Equal(t, requestID, record.RequestID)
	require.Equal(t, "filter_rejected", record.Kind)
	require.Equal(t, "Qwen/Test", record.Model)
	require.Equal(t, "/v1/chat/completions", record.Path)
	require.Contains(t, string(record.Body), `"unsupported_field": true`)
	require.Empty(t, record.BodyBase64)
}

func TestCaptureAllAttemptsFailedRequestWritesSeparateFile(t *testing.T) {
	captureDir := t.TempDir()
	setRequestCaptureStore(&requestCaptureStore{dir: captureDir})
	t.Cleanup(func() { setRequestCaptureStore(nil) })

	ctx, requestID := ensureRequestLogContext(t.Context())
	captureAllAttemptsFailedRequest(ctx, "escrow-7", user.InferenceParams{
		Model:       "Qwen/Test",
		Prompt:      []byte(`{"model":"Qwen/Test","stream":true,"messages":[{"role":"user","content":"hello"}]}`),
		InputLength: 81,
		MaxTokens:   256,
		StartedAt:   time.Now().Unix(),
		Stream:      true,
	}, errTestAllAttemptsFailed{})

	record := requireSingleCapturedRequest(t, captureDir, "all_attempts_failed")
	require.Equal(t, requestID, record.RequestID)
	require.Equal(t, "all_attempts_failed", record.Kind)
	require.Equal(t, "Qwen/Test", record.Model)
	require.Equal(t, "escrow-7", record.Escrow)
	require.True(t, record.Stream)
	require.Contains(t, record.Error, "all attempts failed")
	require.Contains(t, string(record.Body), `"stream": true`)
}

func TestConfigureRequestCaptureStoreDefaultsUnderGatewayDBDirectory(t *testing.T) {
	baseStorageDir := t.TempDir()
	t.Setenv("DEVSHARD_REQUEST_CAPTURE_DIR", "")
	t.Setenv("DEVSHARD_REQUEST_CAPTURE_DISABLED", "")
	t.Cleanup(func() { setRequestCaptureStore(nil) })

	configureRequestCaptureStore(baseStorageDir)

	store := currentRequestCaptureStore()
	require.NotNil(t, store)
	require.Equal(t, filepath.Join(baseStorageDir, requestCaptureDirName), store.dir)
}

func requireSingleCapturedRequest(t *testing.T, captureDir, kind string) capturedChatRequest {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(captureDir, kind))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	body, err := os.ReadFile(filepath.Join(captureDir, kind, entries[0].Name()))
	require.NoError(t, err)
	var record capturedChatRequest
	require.NoError(t, json.Unmarshal(body, &record))
	return record
}

type errTestAllAttemptsFailed struct{}

func (errTestAllAttemptsFailed) Error() string {
	return "all attempts failed"
}
