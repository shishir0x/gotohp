package core

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// writeTempFile creates a temporary file containing n zero bytes.
func writeTempUploadTestFile(t *testing.T, n int) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "upload-test-*.bin")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if n > 0 {
		if _, err := f.Write(make([]byte, n)); err != nil {
			t.Fatalf("write temp file: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}
	return f.Name()
}

// roundTripFunc lets us use a plain function as an http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// mockUploadAPI builds a minimal *Api whose HTTP client redirects to srv.
func mockUploadAPI(t *testing.T, srv *httptest.Server) *Api {
	t.Helper()
	client, err := NewHTTPClientWithProxy("")
	if err != nil {
		t.Fatalf("NewHTTPClientWithProxy: %v", err)
	}
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req2 := req.Clone(req.Context())
		req2.URL.Host = srv.Listener.Addr().String()
		req2.URL.Scheme = "http"
		return http.DefaultTransport.RoundTrip(req2)
	})
	return &Api{
		client:            client,
		authResponseCache: map[string]string{"Expiry": "9999999999", "Auth": "fake-token"},
		language:          "en",
		userAgent:         "test/1.0",
	}
}

// sha1UploadTestFileB64 computes the base64-encoded SHA-1 of a file.
func sha1UploadTestFileB64(t *testing.T, path string) string {
	t.Helper()
	hash, err := CalculateSHA1(context.Background(), path)
	if err != nil {
		t.Fatalf("sha1: %v", err)
	}
	return base64.StdEncoding.EncodeToString(hash)
}

// buildMinimalScottyBody returns the smallest byte sequence accepted by
// ParseScottyFinalizeToken (field1=varint(2), field2=bytes([0x01])).
func buildMinimalScottyBody() []byte {
	return []byte{
		0x08, 0x02,       // field 1 = varint 2
		0x12, 0x01, 0x01, // field 2 = bytes {0x01}
	}
}

// ── ParseRetryAfter ────────────────────────────────────────────────────────────

func TestParseRetryAfter_IntegerSeconds(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "42")
	d := ParseRetryAfter(h)
	if d != 42*time.Second {
		t.Fatalf("want 42s, got %v", d)
	}
}

func TestParseRetryAfter_AbsentHeader(t *testing.T) {
	d := ParseRetryAfter(http.Header{})
	if d != 0 {
		t.Fatalf("want 0, got %v", d)
	}
}

func TestParseRetryAfter_NegativeIsZero(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "-5")
	d := ParseRetryAfter(h)
	if d != 0 {
		t.Fatalf("want 0, got %v", d)
	}
}

// ── ShouldRetryUpload ─────────────────────────────────────────────────────────

func TestShouldRetryUpload_RetryableCodes(t *testing.T) {
	for _, code := range []int{408, 429, 500, 502, 503, 504} {
		retryable, _ := ShouldRetryUpload(code)
		if !retryable {
			t.Errorf("status %d should be retryable", code)
		}
	}
}

func TestShouldRetryUpload_PermanentCodes(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 410} {
		retryable, _ := ShouldRetryUpload(code)
		if retryable {
			t.Errorf("status %d should NOT be retryable", code)
		}
	}
}

// ── Permanent 4xx stops the retry loop ────────────────────────────────────────

func TestUploadFileWithProgress_PermanentErrorStopsRetry(t *testing.T) {
	putCalls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("X-GUploader-UploadID", fmt.Sprintf("session-%d", putCalls+1))
			w.WriteHeader(http.StatusOK)
		} else if r.Method == http.MethodPut {
			putCalls++
			// 400 is permanent and must not be retried
			http.Error(w, "bad request", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	api := mockUploadAPI(t, srv)
	tmpFile := writeTempUploadTestFile(t, 1024)
	sha1B64 := sha1UploadTestFileB64(t, tmpFile)
	fi, _ := os.Stat(tmpFile)

	_, err := api.UploadFileWithProgress(context.Background(), tmpFile, sha1B64, fi.Size(), nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var permanent *PermanentUploadError
	if !errors.As(err, &permanent) {
		t.Fatalf("expected *PermanentUploadError, got %T: %v", err, err)
	}
	if permanent.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", permanent.StatusCode)
	}
	if putCalls != 1 {
		t.Errorf("expected exactly 1 PUT (no retries on 400), got %d", putCalls)
	}
}

// ── Fresh token per attempt ────────────────────────────────────────────────────

func TestUploadFileWithProgress_AcquiresFreshTokenPerAttempt(t *testing.T) {
	// First PUT returns 503 (retryable); second PUT succeeds.
	tokenCalls := 0
	putCalls := 0
	scottyBody := buildMinimalScottyBody()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			tokenCalls++
			w.Header().Set("X-GUploader-UploadID", fmt.Sprintf("session-%d", tokenCalls))
			w.WriteHeader(http.StatusOK)
		} else if r.Method == http.MethodPut {
			putCalls++
			if putCalls == 1 {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(scottyBody)
		}
	}))
	defer srv.Close()

	api := mockUploadAPI(t, srv)
	tmpFile := writeTempUploadTestFile(t, 512)
	sha1B64 := sha1UploadTestFileB64(t, tmpFile)
	fi, _ := os.Stat(tmpFile)

	_, err := api.UploadFileWithProgress(context.Background(), tmpFile, sha1B64, fi.Size(), nil)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}

	// Each PUT attempt must have used its own fresh token.
	if tokenCalls != putCalls {
		t.Errorf("token acquisitions (%d) should equal PUT attempts (%d)", tokenCalls, putCalls)
	}
	if tokenCalls < 2 {
		t.Errorf("expected ≥2 token calls (one per attempt), got %d", tokenCalls)
	}
}

// ── Retry count is bounded ────────────────────────────────────────────────────

func TestUploadFileWithProgress_RetryCountBounded(t *testing.T) {
	putCalls := int32(0)
	cfg := DefaultRetryConfig()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("X-GUploader-UploadID", fmt.Sprintf("s-%d", atomic.AddInt32(&putCalls, 0)+1))
			w.WriteHeader(http.StatusOK)
		} else if r.Method == http.MethodPut {
			atomic.AddInt32(&putCalls, 1)
			http.Error(w, "error", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	api := mockUploadAPI(t, srv)
	tmpFile := writeTempUploadTestFile(t, 256)
	sha1B64 := sha1UploadTestFileB64(t, tmpFile)
	fi, _ := os.Stat(tmpFile)

	_, err := api.UploadFileWithProgress(context.Background(), tmpFile, sha1B64, fi.Size(), nil)
	if err == nil {
		t.Fatal("expected failure after exhausting retries")
	}

	maxExpected := int32(cfg.MaxRetries + 1)
	if putCalls > maxExpected {
		t.Errorf("too many PUT attempts: got %d, max allowed %d", putCalls, maxExpected)
	}
}

// ── Content-Length is set explicitly ─────────────────────────────────────────

func TestUploadFileWithProgress_SetsContentLength(t *testing.T) {
	var receivedContentLength int64 = -999
	scottyBody := buildMinimalScottyBody()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("X-GUploader-UploadID", "session-cl")
			w.WriteHeader(http.StatusOK)
		} else if r.Method == http.MethodPut {
			receivedContentLength = r.ContentLength
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(scottyBody)
		}
	}))
	defer srv.Close()

	api := mockUploadAPI(t, srv)
	const fileBytes = 4096
	tmpFile := writeTempUploadTestFile(t, fileBytes)
	sha1B64 := sha1UploadTestFileB64(t, tmpFile)

	_, err := api.UploadFileWithProgress(context.Background(), tmpFile, sha1B64, fileBytes, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedContentLength != fileBytes {
		t.Errorf("Content-Length on PUT: got %d, want %d", receivedContentLength, fileBytes)
	}
}

// ── Retry-After is honoured ───────────────────────────────────────────────────

func TestUploadFileWithProgress_RespectsRetryAfter(t *testing.T) {
	callTimes := []time.Time{}
	scottyBody := buildMinimalScottyBody()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("X-GUploader-UploadID", "session-ra")
			w.WriteHeader(http.StatusOK)
		} else if r.Method == http.MethodPut {
			callTimes = append(callTimes, time.Now())
			if len(callTimes) == 1 {
				w.Header().Set("Retry-After", "1") // 1 second
				http.Error(w, "rate limited", http.StatusTooManyRequests)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(scottyBody)
		}
	}))
	defer srv.Close()

	api := mockUploadAPI(t, srv)
	tmpFile := writeTempUploadTestFile(t, 256)
	sha1B64 := sha1UploadTestFileB64(t, tmpFile)
	fi, _ := os.Stat(tmpFile)

	_, err := api.UploadFileWithProgress(context.Background(), tmpFile, sha1B64, fi.Size(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(callTimes) < 2 {
		t.Fatalf("expected 2 PUT attempts, got %d", len(callTimes))
	}
	gap := callTimes[1].Sub(callTimes[0])
	if gap < 900*time.Millisecond {
		t.Errorf("Retry-After gap too short: %v (want >= 1s)", gap)
	}
}

// ── Context cancellation stops retries ───────────────────────────────────────

func TestUploadFileWithProgress_ContextCancellationStopsRetries(t *testing.T) {
	var putCalls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("X-GUploader-UploadID", "session-cancel")
			w.WriteHeader(http.StatusOK)
		} else if r.Method == http.MethodPut {
			atomic.AddInt32(&putCalls, 1)
			http.Error(w, "error", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	api := mockUploadAPI(t, srv)
	tmpFile := writeTempUploadTestFile(t, 256)
	sha1B64 := sha1UploadTestFileB64(t, tmpFile)
	fi, _ := os.Stat(tmpFile)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for atomic.LoadInt32(&putCalls) == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	_, err := api.UploadFileWithProgress(ctx, tmpFile, sha1B64, fi.Size(), nil)
	if err == nil {
		t.Fatal("expected error after cancel, got nil")
	}
	if atomic.LoadInt32(&putCalls) > 2 {
		t.Errorf("too many PUT calls after cancel: %d", putCalls)
	}
}

// ── UploadFile public wrapper ─────────────────────────────────────────────────

func TestUploadFile_NewSignatureSucceeds(t *testing.T) {
	scottyBody := buildMinimalScottyBody()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("X-GUploader-UploadID", "session-uf")
			w.WriteHeader(http.StatusOK)
		} else if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(scottyBody)
		}
	}))
	defer srv.Close()

	api := mockUploadAPI(t, srv)
	tmpFile := writeTempUploadTestFile(t, 128)
	sha1B64 := sha1UploadTestFileB64(t, tmpFile)
	fi, _ := os.Stat(tmpFile)

	tok, err := api.UploadFile(context.Background(), tmpFile, sha1B64, fi.Size())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tok.Raw) == 0 {
		t.Error("expected non-empty finalize token")
	}
}

// ── Live photo component acquires fresh token per attempt ──────────────────────

func TestUploadLivePhotoComponent_AcquiresFreshTokenPerAttempt(t *testing.T) {
	tokenCalls := int32(0)
	putCalls := int32(0)
	scottyBody := buildMinimalScottyBody()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			n := atomic.AddInt32(&tokenCalls, 1)
			w.Header().Set("X-GUploader-UploadID", fmt.Sprintf("lp-session-%d", n))
			w.WriteHeader(http.StatusOK)
		} else if r.Method == http.MethodPut {
			n := atomic.AddInt32(&putCalls, 1)
			if n == 1 {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(scottyBody)
		}
	}))
	defer srv.Close()

	api := mockUploadAPI(t, srv)
	tmpFile := writeTempUploadTestFile(t, 512)
	fi, _ := os.Stat(tmpFile)
	hash := make([]byte, 20)

	_, err := uploadLivePhotoComponent(
		context.Background(), api,
		tmpFile, tmpFile,
		fi.Size(), hash,
		0, fi.Size(),
		0, filepath.Base(tmpFile),
		NopReporter{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tc := atomic.LoadInt32(&tokenCalls)
	pc := atomic.LoadInt32(&putCalls)
	if tc != pc {
		t.Errorf("token acquisitions (%d) must equal PUT attempts (%d)", tc, pc)
	}
	if tc < 2 {
		t.Errorf("expected >=2 token calls, got %d", tc)
	}
}
