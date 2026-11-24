package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"vedetta/internal/agent"
)

// A search outlives the connection that started it: a phone that switches its
// screen off drops the stream, and that must not throw the research away. Each
// search runs here on its own context and keeps its events, so a client can come
// back and read them from where it stopped.

// runRetention is how long a finished search stays readable, so a client coming
// back gets the last events; later it finds the answer in the database.
const runRetention = 5 * time.Minute

// runEndEvent closes the stream of a finished search. A stream that stops without
// it was cut, and the client can ask for the rest.
const runEndEvent = "end"

type searchRun struct {
	query  string
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	frames   [][]byte
	finished bool
	changed  chan struct{}
}

func (run *searchRun) add(frame []byte) {
	run.mu.Lock()
	defer run.mu.Unlock()
	run.frames = append(run.frames, frame)
	close(run.changed)
	run.changed = make(chan struct{})
}

// since returns the events after the first `from`, whether the search is over,
// and a channel closed at the next change.
func (run *searchRun) since(from int) ([][]byte, bool, <-chan struct{}) {
	run.mu.Lock()
	defer run.mu.Unlock()
	if from < 0 {
		from = 0
	}
	if from > len(run.frames) {
		from = len(run.frames)
	}
	return run.frames[from:len(run.frames):len(run.frames)], run.finished, run.changed
}

func (run *searchRun) isFinished() bool {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.finished
}

// stream writes the events from `from` on until the search ends or the client
// goes away. Leaving does not touch the search.
func (run *searchRun) stream(ctx context.Context, sse *SSEWriter, from int) {
	for {
		frames, finished, changed := run.since(from)
		if len(frames) > 0 {
			if err := sse.WriteFrames(frames); err != nil {
				return
			}
			from += len(frames)
		}
		if finished {
			return
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return
		}
	}
}

type runRegistry struct {
	base context.Context
	stop context.CancelFunc
	wg   sync.WaitGroup

	mu        sync.Mutex
	bySession map[string]*searchRun
}

func newRunRegistry() *runRegistry {
	base, stop := context.WithCancel(context.Background())
	return &runRegistry{base: base, stop: stop, bySession: make(map[string]*searchRun)}
}

// start launches exec in the background, or returns nil when a search of the session is
// still running. timeout is a last-resort limit: the
// engine has its own, and it was the connection that stopped a search before.
func (reg *runRegistry) start(query, sessionID string, timeout time.Duration, exec func(context.Context, agent.Emitter) error) *searchRun {
	ctx, cancel := context.WithTimeout(reg.base, timeout)
	run := &searchRun{query: query, cancel: cancel, done: make(chan struct{}), changed: make(chan struct{})}
	if sessionID != "" && !reg.claim(sessionID, run) {
		cancel()
		return nil
	}

	emit := func(ev agent.Event) error {
		frame, err := formatSSE(string(ev.Type), ev.Data)
		if err != nil {
			return err
		}
		if ev.Type == agent.EventSession {
			if data, ok := ev.Data.(map[string]string); ok && data["session_id"] != "" {
				reg.bind(data["session_id"], run)
			}
		}
		run.add(frame)
		return nil
	}

	reg.wg.Add(1)
	go func() {
		defer reg.wg.Done()
		defer cancel()
		defer reg.finish(run)
		// Out of the HTTP handler nothing recovers a panic for us, and one search
		// must not bring the server down.
		defer func() {
			if p := recover(); p != nil {
				log.Printf("[search] panic: %v", p)
				_ = emit(agent.Event{Type: agent.EventError, Data: map[string]string{"error": fmt.Sprint(p)}})
			}
		}()
		err := exec(ctx, emit)
		if err != nil && !errors.Is(ctx.Err(), context.Canceled) {
			_ = emit(agent.Event{Type: agent.EventError, Data: map[string]string{"error": err.Error()}})
		}
	}()
	return run
}

// claim binds the session to run, unless a search of it is still running.
func (reg *runRegistry) claim(sessionID string, run *searchRun) bool {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if old := reg.bySession[sessionID]; old != nil && !old.isFinished() {
		return false
	}
	reg.bySession[sessionID] = run
	return true
}

func (reg *runRegistry) bind(sessionID string, run *searchRun) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.bySession[sessionID] = run
}

func (reg *runRegistry) finish(run *searchRun) {
	frame, _ := formatSSE(runEndEvent, "{}")
	run.mu.Lock()
	run.frames = append(run.frames, frame)
	run.finished = true
	close(run.changed)
	run.changed = make(chan struct{})
	run.mu.Unlock()
	close(run.done)

	time.AfterFunc(runRetention, func() {
		reg.mu.Lock()
		defer reg.mu.Unlock()
		for id, r := range reg.bySession {
			if r == run {
				delete(reg.bySession, id)
			}
		}
	})
}

func (reg *runRegistry) get(sessionID string) *searchRun {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.bySession[sessionID]
}

// cancelSession stops the search of a session and waits for it to finish saving,
// so that a session being deleted does not get a message written afterwards.
func (reg *runRegistry) cancelSession(sessionID string, wait time.Duration) {
	run := reg.get(sessionID)
	if run == nil {
		return
	}
	run.cancel()
	select {
	case <-run.done:
	case <-time.After(wait):
		log.Printf("[search] session=%s still stopping after %v", sessionID, wait)
	}
}

// cancelAll stops every running search and waits for each up to wait (map R7:
// before deleting every session).
func (reg *runRegistry) cancelAll(wait time.Duration) {
	reg.mu.Lock()
	ids := make([]string, 0, len(reg.bySession))
	for id := range reg.bySession {
		ids = append(ids, id)
	}
	reg.mu.Unlock()
	for _, id := range ids {
		reg.cancelSession(id, wait)
	}
}

// shutdown stops every search, so each saves what it has, and waits within ctx.
func (reg *runRegistry) shutdown(ctx context.Context) {
	reg.stop()
	finished := make(chan struct{})
	go func() {
		reg.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-ctx.Done():
	}
}
