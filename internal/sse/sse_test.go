package sse_test

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/require"

	"github.com/eeee0717/llm-gateway/internal/sse"
)

func TestReaderSplitsEventsAndKeepsRawBytes(t *testing.T) {
	r := sse.NewReader(strings.NewReader("data: {\"a\":1}\n\n: keep-alive\n\ndata: [DONE]\r\n\r\n"))

	ev, err := r.Next()
	require.NoError(t, err)
	require.Equal(t, "data: {\"a\":1}\n\n", string(ev.Raw))
	require.Equal(t, `{"a":1}`, string(ev.Data))

	ev, err = r.Next()
	require.NoError(t, err)
	require.Equal(t, ": keep-alive\n\n", string(ev.Raw))
	require.Empty(t, ev.Data)

	ev, err = r.Next()
	require.NoError(t, err)
	require.Equal(t, "data: [DONE]\r\n\r\n", string(ev.Raw))
	require.Equal(t, "[DONE]", string(ev.Data))

	_, err = r.Next()
	require.ErrorIs(t, err, io.EOF)
}

func TestReaderJoinsMultilineData(t *testing.T) {
	r := sse.NewReader(strings.NewReader("event: message\ndata: first\ndata:second\n\n"))

	ev, err := r.Next()
	require.NoError(t, err)
	require.Equal(t, "first\nsecond", string(ev.Data))
}

func TestReaderDropsIncompleteEventAtEOF(t *testing.T) {
	r := sse.NewReader(strings.NewReader("data: complete\n\ndata: truncated\n"))

	ev, err := r.Next()
	require.NoError(t, err)
	require.Equal(t, "complete", string(ev.Data))

	_, err = r.Next()
	require.ErrorIs(t, err, io.EOF)
}

func TestReaderReturnsConnectionError(t *testing.T) {
	r := sse.NewReader(io.MultiReader(
		strings.NewReader("data: partial\n"),
		iotest.ErrReader(io.ErrUnexpectedEOF),
	))

	_, err := r.Next()
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}
