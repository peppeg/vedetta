package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_TemperatureZero(t *testing.T) {
	var receivedBody map[string]interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &receivedBody)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	client := NewClient(server.URL, "key", "test-model")

	// Call with explicit Temperature 0.0
	zero := 0.0
	err := client.StreamChatWithOptions(context.Background(), []Message{
		{Role: "user", Content: "hello"},
	}, ChatOptions{
		Temperature: &zero,
	}, func(token StreamToken) error {
		return nil
	})

	if err != nil {
		t.Fatalf("StreamChatWithOptions failed: %v", err)
	}

	tempVal, exists := receivedBody["temperature"]
	if !exists {
		t.Fatalf("expected 'temperature' field in request body, but was missing: %v", receivedBody)
	}

	tempFloat, ok := tempVal.(float64)
	if !ok || tempFloat != 0.0 {
		t.Fatalf("expected temperature 0.0, got %v (%T)", tempVal, tempVal)
	}
}

func TestClient_DefaultTemperature(t *testing.T) {
	var receivedBody map[string]interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &receivedBody)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	client := NewClient(server.URL, "key", "test-model")

	// Call without setting Temperature (nil)
	err := client.StreamChatWithOptions(context.Background(), []Message{
		{Role: "user", Content: "hello"},
	}, ChatOptions{}, func(token StreamToken) error {
		return nil
	})

	if err != nil {
		t.Fatalf("StreamChatWithOptions failed: %v", err)
	}

	tempVal, exists := receivedBody["temperature"]
	if !exists {
		t.Fatalf("expected 'temperature' field in request body, but was missing")
	}

	tempFloat, ok := tempVal.(float64)
	if !ok || tempFloat != 0.6 {
		t.Fatalf("expected default temperature 0.6, got %v", tempVal)
	}
}
