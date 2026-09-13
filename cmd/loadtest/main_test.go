package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// -extra 用来加上游特有的字段（例如 enable_thinking），但压测的前提是流式，
// 所以核心字段后写，extra 覆盖不掉。
func TestExtraFieldsCannotBreakTheProbe(t *testing.T) {
	got, err := requestBody("qwen", `{"max_tokens":1,"enable_thinking":false,"stream":false}`)

	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(got, &body))
	require.Equal(t, "qwen", body["model"])
	require.Equal(t, true, body["stream"])
	require.Equal(t, float64(1), body["max_tokens"])
	require.Equal(t, false, body["enable_thinking"])
	require.NotEmpty(t, body["messages"])
}

func TestExtraIsOptional(t *testing.T) {
	got, err := requestBody("qwen", "")

	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(got, &body))
	require.Equal(t, true, body["stream"])
}
