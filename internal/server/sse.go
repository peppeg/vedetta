package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

type SSEWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	mu      sync.Mutex
}

func NewSSEWriter(w http.ResponseWriter) (*SSEWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("streaming unsupported by response writer")
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher.Flush()

	return &SSEWriter{
		w:       w,
		flusher: flusher,
	}, nil
}

func (s *SSEWriter) SendEvent(eventType string, data interface{}) error {
	frame, err := formatSSE(eventType, data)
	if err != nil {
		return err
	}
	return s.WriteFrames([][]byte{frame})
}

// WriteFrames sends events already formatted by formatSSE.
func (s *SSEWriter) WriteFrames(frames [][]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, frame := range frames {
		if _, err := s.w.Write(frame); err != nil {
			return err
		}
	}
	s.flusher.Flush()
	return nil
}

func formatSSE(eventType string, data interface{}) ([]byte, error) {
	var payload []byte
	switch v := data.(type) {
	case string:
		payload = []byte(v)
	case []byte:
		payload = v
	default:
		var err error
		payload, err = json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal sse data: %w", err)
		}
	}

	// Format SSE strictly according to W3C EventSource specification:
	// each newline in multi-line data must have its own 'data: ' prefix
	var sb strings.Builder
	sb.WriteString("event: ")
	sb.WriteString(eventType)
	sb.WriteByte('\n')

	lines := strings.Split(string(payload), "\n")
	for _, line := range lines {
		sb.WriteString("data: ")
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	sb.WriteByte('\n')

	return []byte(sb.String()), nil
}
