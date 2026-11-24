package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSSEWriter_MultilineEvent(t *testing.T) {
	rec := httptest.NewRecorder()
	writer, err := NewSSEWriter(rec)
	if err != nil {
		t.Fatalf("failed to create SSEWriter: %v", err)
	}

	multilineText := "Riga 1\nRiga 2\nRiga 3"
	err = writer.SendEvent("token", multilineText)
	if err != nil {
		t.Fatalf("SendEvent failed: %v", err)
	}

	body := rec.Body.String()
	expected := "event: token\ndata: Riga 1\ndata: Riga 2\ndata: Riga 3\n\n"
	if !strings.HasSuffix(body, expected) {
		t.Errorf("unexpected SSE output:\nwant: %q\ngot:  %q", expected, body)
	}
}
