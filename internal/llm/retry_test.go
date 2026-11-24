package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const okStream = "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n"

// fakeClock replaces the client's clock and sleep, so no test waits for real.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	waits []time.Duration
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waits = append(f.waits, d)
	f.t = f.t.Add(d)
	return nil
}

func (f *fakeClock) slept() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.waits...)
}

func newTestClient(url string) (*Client, *fakeClock) {
	clock := &fakeClock{t: time.Now()}
	c := NewClient(url, "key", "test-model")
	c.now = clock.now
	c.sleep = clock.sleep
	c.jitter = func() float64 { return 0.5 } // no jitter: waits are exactly 1 s, 2 s, 4 s, 8 s
	return c, clock
}

// scripted answers the i-th request with steps[i] (the last step repeats).
func scripted(t *testing.T, steps ...func(w http.ResponseWriter)) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(atomic.AddInt32(&calls, 1)) - 1
		if i >= len(steps) {
			i = len(steps) - 1
		}
		steps[i](w)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func status(code int, header, body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		if header != "" {
			k, v, _ := strings.Cut(header, ": ")
			w.Header().Set(k, v)
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
}

func ok(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(okStream))
}

func chat(ctx context.Context, c *Client) (string, error) {
	var out strings.Builder
	err := c.StreamChatWithOptions(ctx, []Message{{Role: "user", Content: "hi"}}, ChatOptions{}, func(tok StreamToken) error {
		out.WriteString(tok.Text)
		return nil
	})
	return out.String(), err
}

func assertStatusErr(t *testing.T, err error, code, attempts int) {
	t.Helper()
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("expected *StatusError, got %v", err)
	}
	if se.Code != code || se.Attempts != attempts {
		t.Fatalf("expected status %d after %d attempts, got %d after %d", code, attempts, se.Code, se.Attempts)
	}
}

// a
func TestRetry_503ThenOK(t *testing.T) {
	srv, calls := scripted(t, status(503, "", "busy"), status(503, "", "busy"), ok)
	c, clock := newTestClient(srv.URL)
	out, err := chat(context.Background(), c)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if out != "OK" {
		t.Fatalf("tokens delivered %q, want %q", out, "OK")
	}
	if *calls != 3 {
		t.Fatalf("calls = %d, want 3", *calls)
	}
	if w := clock.slept(); len(w) != 2 || w[0] != time.Second || w[1] != 2*time.Second {
		t.Fatalf("waits = %v, want [1s 2s]", w)
	}
}

// b
func TestRetry_RetryAfterHeader(t *testing.T) {
	srv, _ := scripted(t, status(429, "Retry-After: 2", "slow down"), ok)
	c, clock := newTestClient(srv.URL)
	if _, err := chat(context.Background(), c); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if w := clock.slept(); len(w) != 1 || w[0] != 2*time.Second {
		t.Fatalf("waits = %v, want [2s]", w)
	}
}

// c
func TestRetry_HintOverCapGivesUp(t *testing.T) {
	body := `{"error":{"message":"No deployments available for selected model, Try again in 137.0 seconds."}}`
	srv, calls := scripted(t, status(429, "", body), ok)
	c, clock := newTestClient(srv.URL)
	_, err := chat(context.Background(), c)
	assertStatusErr(t, err, 429, 1)
	if *calls != 1 || len(clock.slept()) != 0 {
		t.Fatalf("calls = %d, waits = %v: want 1 call and no wait", *calls, clock.slept())
	}
}

// d
func TestRetry_WaitPastDeadlineGivesUp(t *testing.T) {
	srv, calls := scripted(t, status(503, "Retry-After: 10", "busy"), ok)
	c, _ := newTestClient(srv.URL)
	c.now = time.Now // the deadline is real
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := chat(ctx, c)
	assertStatusErr(t, err, 503, 1)
	if *calls != 1 {
		t.Fatalf("calls = %d, want 1", *calls)
	}
}

// e
func TestRetry_ClientErrorsNotRetried(t *testing.T) {
	for _, code := range []int{400, 401, 404} {
		srv, calls := scripted(t, status(code, "", "no"), ok)
		c, _ := newTestClient(srv.URL)
		_, err := chat(context.Background(), c)
		assertStatusErr(t, err, code, 1)
		if *calls != 1 {
			t.Fatalf("status %d: calls = %d, want 1", code, *calls)
		}
	}
}

// f
func TestRetry_BrokenStreamNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "1000") // promise more than is sent
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n"))
	}))
	defer srv.Close()
	c, _ := newTestClient(srv.URL)
	out, err := chat(context.Background(), c)
	if err == nil {
		t.Fatal("expected a stream error")
	}
	if calls != 1 || out != "par" {
		t.Fatalf("calls = %d, out = %q: want 1 call and the partial token once", calls, out)
	}
}

// g
func TestRetry_CanceledDuringWait(t *testing.T) {
	srv, calls := scripted(t, status(503, "", "busy"), ok)
	c, _ := newTestClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	c.sleep = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	_, err := chat(ctx, c)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if *calls != 1 {
		t.Fatalf("calls = %d, want 1", *calls)
	}
}

// h
func TestRetry_AlwaysFailing(t *testing.T) {
	srv, calls := scripted(t, status(503, "", "busy"))
	c, clock := newTestClient(srv.URL)
	_, err := chat(context.Background(), c)
	assertStatusErr(t, err, 503, 5)
	if *calls != 5 || len(clock.slept()) != 4 {
		t.Fatalf("calls = %d, waits = %v: want 5 calls and 4 waits", *calls, clock.slept())
	}

	srv2, calls2 := scripted(t, status(503, "Retry-After: 1", "busy"))
	c2, _ := newTestClient(srv2.URL)
	c2.WithRetryPolicy(NewRetryPolicy(0, 30))
	_, err = chat(context.Background(), c2)
	assertStatusErr(t, err, 503, 1)
	if *calls2 != 1 {
		t.Fatalf("retries off: calls = %d, want 1", *calls2)
	}
	if !c2.pausedUntil.IsZero() {
		t.Fatal("retries off: the client must not pause")
	}
}

// i
func TestRetry_SharedPause(t *testing.T) {
	srv, calls := scripted(t, status(429, "Retry-After: 3", "slow down"), ok)
	c, clock := newTestClient(srv.URL)
	c.now = time.Now
	c.sleep = func(ctx context.Context, d time.Duration) error { return clock.sleep(ctx, d) }

	// The first call has 2 s left: it cannot wait 3 s, gives up, and leaves the pause.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, err := chat(ctx, c)
	cancel()
	assertStatusErr(t, err, 429, 1)

	// The second call waits for the pause before calling, then succeeds.
	if _, err := chat(context.Background(), c); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	w := clock.slept()
	if len(w) != 1 || w[0] < 2500*time.Millisecond || w[0] > 3*time.Second {
		t.Fatalf("waits = %v, want one wait of about 3s", w)
	}
	if *calls != 2 {
		t.Fatalf("calls = %d, want 2", *calls)
	}
}

func TestRetry_PauseOverCapGivesUpWithoutCalling(t *testing.T) {
	body := `{"error":{"message":"Try again in 137.0 seconds"}}`
	srv, calls := scripted(t, status(429, "", body), ok)
	c, _ := newTestClient(srv.URL)
	_, err := chat(context.Background(), c)
	assertStatusErr(t, err, 429, 1)
	_, err = chat(context.Background(), c)
	assertStatusErr(t, err, 429, 0)
	if *calls != 1 {
		t.Fatalf("calls = %d, want 1: the second call must not reach the server", *calls)
	}
}

func TestRetry_ConcurrentCallsRace(t *testing.T) {
	srv, _ := scripted(t, status(503, "Retry-After: 1", "busy"), status(503, "Retry-After: 1", "busy"), ok)
	c, _ := newTestClient(srv.URL)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = chat(context.Background(), c)
		}()
	}
	wg.Wait()
}

// j
func TestRetry_ReasoningEffortSelfHealNotCounted(t *testing.T) {
	srv, calls := scripted(t, status(400, "", `{"error":"unsupported reasoning_effort"}`), ok)
	c, clock := newTestClient(srv.URL)
	err := c.StreamChatWithOptions(context.Background(), []Message{{Role: "user", Content: "hi"}},
		ChatOptions{Effort: EffortHigh}, func(StreamToken) error { return nil })
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if *calls != 2 || len(clock.slept()) != 0 {
		t.Fatalf("calls = %d, waits = %v: want 2 calls and no wait", *calls, clock.slept())
	}
}

// k
func TestRetry_DroppedConnection(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		ok(w)
	}))
	defer srv.Close()
	c, _ := newTestClient(srv.URL)
	if _, err := chat(context.Background(), c); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

// l
func TestRetry_ErrorTextUnchanged(t *testing.T) {
	srv, _ := scripted(t, status(503, "", " busy \n"))
	c, _ := newTestClient(srv.URL)
	_, err := chat(context.Background(), c)
	if got, want := err.Error(), "llm returned error status 503: busy"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestServerHint(t *testing.T) {
	now := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		header, body string
		want         time.Duration
		ok           bool
	}{
		{"7", "", 7 * time.Second, true},
		{"1.5", "", 1500 * time.Millisecond, true},
		{now.Add(20 * time.Second).Format(http.TimeFormat), "", 20 * time.Second, true},
		{"", "No deployments available, Try again in 137.0 seconds.", 137 * time.Second, true},
		{"", "please TRY AGAIN IN 5s", 5 * time.Second, true},
		{"", "busy", 0, false},
	}
	for _, tc := range cases {
		h := http.Header{}
		if tc.header != "" {
			h.Set("Retry-After", tc.header)
		}
		got, ok := serverHint(h, tc.body, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("serverHint(%q, %q) = %v, %v; want %v, %v", tc.header, tc.body, got, ok, tc.want, tc.ok)
		}
	}
}
