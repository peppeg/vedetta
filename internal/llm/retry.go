package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RetryPolicy says how the client retries a chat request before the response
// starts (V25-T7.1, docs/SPEC_T71_RITENTATIVI_LLM.md). Once the server has
// answered 200 the stream is never retried: tokens already reached the caller.
type RetryPolicy struct {
	MaxRetries int           // extra attempts after the first call; 0 = off
	MaxWait    time.Duration // longest single wait; a server asking more ends the retries at once
}

// DefaultRetryPolicy is 4 retries (waits 1, 2, 4, 8 s: about 15 s of outage) with waits of at most 30 s.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxRetries: 4, MaxWait: 30 * time.Second}
}

// NewRetryPolicy builds a policy from the configuration values
// (VEDETTA_LLM_RETRIES, VEDETTA_LLM_RETRY_MAX_WAIT_SECS).
func NewRetryPolicy(retries, maxWaitSecs int) RetryPolicy {
	if retries < 0 {
		retries = 0
	}
	if maxWaitSecs <= 0 {
		maxWaitSecs = 30
	}
	return RetryPolicy{MaxRetries: retries, MaxWait: time.Duration(maxWaitSecs) * time.Second}
}

// StatusError is a non-200 answer from the LLM server. The text is the same as
// before T7.1, so log searches like "status 503" keep working.
type StatusError struct {
	Code     int
	Body     string
	Attempts int // calls made; 0 when the client gave up during a server-requested pause
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("llm returned error status %d: %s", e.Code, e.Body)
}

func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// retryableTransport reports whether a transport error is a dropped or refused
// connection. Timeouts are not retried: the time is already spent.
func retryableTransport(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// retryInRe matches the LiteLLM hint "Try again in 137.0 seconds".
var retryInRe = regexp.MustCompile(`(?i)try again in ([0-9]+(?:\.[0-9]+)?) ?s`)

// serverHint returns how long the server asks to wait: the Retry-After header
// (seconds or HTTP date), else the LiteLLM text in the body.
func serverHint(h http.Header, body string, now time.Time) (time.Duration, bool) {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
			return time.Duration(secs * float64(time.Second)), true
		}
		if t, err := http.ParseTime(v); err == nil {
			return max(t.Sub(now), 0), true
		}
	}
	if m := retryInRe.FindStringSubmatch(body); m != nil {
		if secs, err := strconv.ParseFloat(m[1], 64); err == nil {
			return time.Duration(secs * float64(time.Second)), true
		}
	}
	return 0, false
}

// backoff is 1 s, 2 s, 4 s, ... for attempt 1, 2, 3, ..., each ±30%.
func backoff(attempt int, jitter func() float64) time.Duration {
	base := time.Second << (attempt - 1)
	return time.Duration(float64(base) * (0.7 + 0.6*jitter()))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func defaultJitter() float64 { return rand.Float64() }

// WithRetryPolicy sets the retry policy and returns the client.
func (c *Client) WithRetryPolicy(p RetryPolicy) *Client {
	c.retry = p
	return c
}

// fitsDeadline reports whether waiting d still leaves the call inside ctx's deadline.
func (c *Client) fitsDeadline(ctx context.Context, d time.Duration) bool {
	dl, ok := ctx.Deadline()
	return !ok || !c.now().Add(d).After(dl)
}

func (c *Client) setPause(until time.Time, se *StatusError) {
	c.pauseMu.Lock()
	defer c.pauseMu.Unlock()
	if until.After(c.pausedUntil) {
		c.pausedUntil = until
		c.pauseErr = se
	}
}

// waitPause waits for a pause the server asked of this client, or gives up with
// the error that started it when the pause is longer than the cap or the deadline.
func (c *Client) waitPause(ctx context.Context) error {
	c.pauseMu.Lock()
	until, se := c.pausedUntil, c.pauseErr
	c.pauseMu.Unlock()
	d := until.Sub(c.now())
	if d <= 0 || se == nil {
		return nil
	}
	if d > c.retry.MaxWait || !c.fitsDeadline(ctx, d) {
		log.Printf("[llm] %s paused by the server for another %s: giving up without calling", c.model, d.Round(time.Second))
		return &StatusError{Code: se.Code, Body: se.Body}
	}
	log.Printf("[llm] %s paused by the server, waiting %s", c.model, d.Round(100*time.Millisecond))
	return c.sleep(ctx, d)
}

// post sends the chat request, retrying refused or dropped connections and
// 429/5xx answers as the policy allows. It returns the response (200 or a
// status that is not retried, body unread) and the number of calls made.
func (c *Client) post(ctx context.Context, endpoint string, payload []byte) (*http.Response, int, error) {
	retriesOn := c.retry.MaxRetries > 0
	for attempt := 1; ; attempt++ {
		if retriesOn {
			if err := c.waitPause(ctx); err != nil {
				return nil, attempt - 1, err
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, attempt, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}

		var failure error
		var hint time.Duration
		var hasHint bool
		resp, err := c.httpClient.Do(req)
		switch {
		case err != nil:
			failure = fmt.Errorf("llm request failed: %w", err)
			if ctx.Err() != nil || !retryableTransport(err) {
				return nil, attempt, failure
			}
		case retryableStatus(resp.StatusCode):
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			se := &StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(body)), Attempts: attempt}
			failure = se
			if hint, hasHint = serverHint(resp.Header, se.Body, c.now()); hasHint && retriesOn {
				c.setPause(c.now().Add(hint), se)
			}
		default:
			if attempt > 1 && resp.StatusCode == http.StatusOK {
				log.Printf("[llm] %s ok after %d attempts", c.model, attempt)
			}
			return resp, attempt, nil
		}

		if attempt > c.retry.MaxRetries {
			if retriesOn {
				log.Printf("[llm] %s giving up after %d attempts: %s", c.model, attempt, truncate(failure.Error(), 200))
			}
			return nil, attempt, failure
		}
		wait := backoff(attempt, c.jitter)
		if hasHint {
			wait = hint
		}
		if wait > c.retry.MaxWait {
			log.Printf("[llm] %s: server asks %s, more than the %s cap: giving up (%s)",
				c.model, wait.Round(time.Second), c.retry.MaxWait, truncate(failure.Error(), 200))
			return nil, attempt, failure
		}
		if !c.fitsDeadline(ctx, wait) {
			log.Printf("[llm] %s: waiting %s would pass the stage deadline: giving up (%s)",
				c.model, wait.Round(100*time.Millisecond), truncate(failure.Error(), 200))
			return nil, attempt, failure
		}
		log.Printf("[llm] %s: %s (attempt %d/%d), retrying in %s",
			c.model, truncate(failure.Error(), 200), attempt, c.retry.MaxRetries+1, wait.Round(100*time.Millisecond))
		if err := c.sleep(ctx, wait); err != nil {
			return nil, attempt, err
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
