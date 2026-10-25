package core

import (
	"compress/gzip"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PermanentUploadError wraps an HTTP error that must not be retried because
// re-sending the same request cannot succeed (e.g. 400 Bad Request, 403
// Forbidden, 404 Not Found, 410 Gone).
type PermanentUploadError struct {
	StatusCode int
	Body       string
}

func (e *PermanentUploadError) Error() string {
	return fmt.Sprintf("permanent upload failure (HTTP %d): %s", e.StatusCode, e.Body)
}

func NewHTTPClientWithProxy(proxyURLStr string) (*http.Client, error) {
	// Create the base transport with default values
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig.InsecureSkipVerify = false

	// Configure connection pooling for concurrent uploads
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 10
	transport.MaxConnsPerHost = 10
	transport.IdleConnTimeout = 90 * time.Second

	// Configure proxy if provided
	if proxyURLStr != "" {
		proxyURL, err := url.Parse(proxyURLStr)
		if err != nil {
			return nil, err
		}
		transport.Proxy = http.ProxyURL(proxyURL)
		transport.TLSClientConfig.InsecureSkipVerify = true
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   0, // No timeout for large uploads - context handles cancellation
	}

	return client, nil
}

// RetryConfig holds configuration for retry behavior
type RetryConfig struct {
	MaxRetries   int
	InitialDelay time.Duration
	MaxDelay     time.Duration
}

// DefaultRetryConfig returns sensible defaults for retries
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:   3,
		InitialDelay: 1 * time.Second,
		MaxDelay:     30 * time.Second,
	}
}

// ShouldRetry determines if an HTTP response warrants a retry
func ShouldRetry(resp *http.Response, err error) bool {
	if err != nil {
		return true // Network errors should be retried
	}
	if resp == nil {
		return true
	}
	// Retry on 5xx server errors and 429 (rate limit)
	return resp.StatusCode >= 500 || resp.StatusCode == 429
}

// ShouldRetryUpload returns (retryable, retryAfter) for an upload PUT response.
// 4xx errors other than 408 (Request Timeout) and 429 (Rate Limit) are
// permanent: the same session token cannot be recovered by retrying.
func ShouldRetryUpload(statusCode int) (retryable bool, retryAfter time.Duration) {
	switch {
	case statusCode == 429:
		return true, 0 // caller parses Retry-After
	case statusCode == 408:
		return true, 0
	case statusCode >= 500:
		return true, 0
	default:
		// 400, 401, 403, 404, 410, etc. — permanent
		return false, 0
	}
}

// ParseRetryAfter reads the Retry-After header value and returns the delay to
// wait. Returns 0 if the header is absent or unparseable.
func ParseRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	// Integer seconds form
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs > 0 {
			return time.Duration(secs) * time.Second
		}
		return 0
	}
	// HTTP-date form
	for _, layout := range []string{time.RFC1123, time.RFC850, time.ANSIC} {
		if t, err := time.Parse(layout, v); err == nil {
			d := time.Until(t)
			if d > 0 {
				return d
			}
			return 0
		}
	}
	return 0
}

// CalculateBackoff returns the delay for a given attempt (exponential backoff with jitter)
func CalculateBackoff(attempt int, config RetryConfig) time.Duration {
	delay := config.InitialDelay * time.Duration(1<<uint(attempt))
	if delay > config.MaxDelay {
		delay = config.MaxDelay
	}
	// Add up to 10% jitter to prevent thundering herd
	jitter := time.Duration(rand.Int63n(int64(delay / 10)))
	return delay + jitter
}

// CheckResponse validates an HTTP response and returns an error if not successful (2xx)
func CheckResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := ReadResponseBody(resp)
	return fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(body))
}

func ReadResponseBody(resp *http.Response) ([]byte, error) {
	var reader io.Reader = resp.Body
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Encoding")), "gzip") {
		gzipReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer func() { _ = gzipReader.Close() }()
		reader = gzipReader
	}
	return io.ReadAll(reader)
}
