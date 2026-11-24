package llm

import (
	"reflect"
	"testing"
)

func TestStreamFilter_SplitOpenTag(t *testing.T) {
	sf := NewStreamFilter()

	// Chunk 1 has partial tag "<thi"
	t1 := sf.Process("<thi", false)
	if len(t1) != 0 {
		t.Fatalf("expected 0 tokens for partial tag, got: %+v", t1)
	}

	// Chunk 2 completes "<think>" tag and adds content + closing tag
	t2 := sf.Process("nk>ragionamento</think>risposta", false)

	expected := []StreamToken{
		{Type: TokenReasoning, Text: "ragionamento"},
		{Type: TokenContent, Text: "risposta"},
	}

	if !reflect.DeepEqual(t2, expected) {
		t.Fatalf("unexpected tokens:\nwant: %+v\ngot:  %+v", expected, t2)
	}
}

func TestStreamFilter_SplitCloseTag(t *testing.T) {
	sf := NewStreamFilter()

	// Chunk 1 starts think and has partial closing tag
	t1 := sf.Process("<think>ragionamento</thi", false)
	expectedT1 := []StreamToken{
		{Type: TokenReasoning, Text: "ragionamento"},
	}
	if !reflect.DeepEqual(t1, expectedT1) {
		t.Fatalf("unexpected tokens in chunk 1:\nwant: %+v\ngot:  %+v", expectedT1, t1)
	}

	// Chunk 2 closes tag and gives final answer
	t2 := sf.Process("nk>risposta finale", false)
	expectedT2 := []StreamToken{
		{Type: TokenContent, Text: "risposta finale"},
	}
	if !reflect.DeepEqual(t2, expectedT2) {
		t.Fatalf("unexpected tokens in chunk 2:\nwant: %+v\ngot:  %+v", expectedT2, t2)
	}
}

func TestStreamFilter_UnclosedThink(t *testing.T) {
	sf := NewStreamFilter()

	tokens := sf.Process("<think>pensiero in corso senza fine", false)
	tokens = append(tokens, sf.Flush()...)

	expected := []StreamToken{
		{Type: TokenReasoning, Text: "pensiero in corso senza fine"},
	}

	if !reflect.DeepEqual(tokens, expected) {
		t.Fatalf("unexpected tokens for unclosed think:\nwant: %+v\ngot:  %+v", expected, tokens)
	}
}

func TestStreamFilter_ExplicitReasoning(t *testing.T) {
	sf := NewStreamFilter()

	tokens := sf.Process("ragionamento nativo da reasoning_content", true)
	expected := []StreamToken{
		{Type: TokenReasoning, Text: "ragionamento nativo da reasoning_content"},
	}

	if !reflect.DeepEqual(tokens, expected) {
		t.Fatalf("unexpected tokens for explicit reasoning:\nwant: %+v\ngot:  %+v", expected, tokens)
	}
}

func TestStreamFilter_NoThinkTag(t *testing.T) {
	sf := NewStreamFilter()

	tokens := sf.Process("semplice risposta diretta senza tag", false)
	tokens = append(tokens, sf.Flush()...)

	expected := []StreamToken{
		{Type: TokenContent, Text: "semplice risposta diretta senza tag"},
	}

	if !reflect.DeepEqual(tokens, expected) {
		t.Fatalf("unexpected tokens for direct text:\nwant: %+v\ngot:  %+v", expected, tokens)
	}
}
