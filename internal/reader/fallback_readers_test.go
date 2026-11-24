package reader

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCompositeReader_FallbackReaders(t *testing.T) {
	long := strings.Repeat("contenuto leggibile ", 30)
	httpR := &mockReader{name: "http", errToRet: errors.New("403")}
	jina := &mockReader{name: "jina-reader", docToRet: &Document{Content: long}}
	c := NewCompositeReader(CompositeReaderConfig{Chain: []Reader{httpR, jina}})

	ctx := WithSessionID(context.Background(), "s1")
	if _, err := c.Read(ctx, "https://example.org/a"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(ctx, "https://example.org/b"); err != nil {
		t.Fatal(err)
	}
	primary, served := c.FallbackReaders("s1")
	if primary != "http" || !reflect.DeepEqual(served, []string{"jina-reader"}) {
		t.Fatalf("got %q %v", primary, served)
	}
	// Another session saw no fallback.
	if _, served := c.FallbackReaders("s2"); len(served) != 0 {
		t.Fatalf("s2: %v", served)
	}
	// Pages read by the first reader are not a fallback.
	ok := NewCompositeReader(CompositeReaderConfig{Chain: []Reader{&mockReader{name: "http", docToRet: &Document{Content: long}}, jina}})
	_, _ = ok.Read(WithSessionID(context.Background(), "s3"), "https://example.org/c")
	if _, served := ok.FallbackReaders("s3"); len(served) != 0 {
		t.Fatalf("s3: %v", served)
	}
}
