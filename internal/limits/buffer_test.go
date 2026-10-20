package limits

import (
	"strings"
	"testing"
)

func TestBufferDrainsButBoundsOutput(t *testing.T) {
	buffer := &Buffer{Limit: 3}
	if n, err := buffer.Write([]byte("abcdef")); err != nil || n != 6 {
		t.Fatal(n, err)
	}
	if buffer.Len() != 3 || !strings.Contains(buffer.String(), "truncated") {
		t.Fatal(buffer.String())
	}
}
