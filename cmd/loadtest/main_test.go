package main

import (
	"encoding/json"
	"io"
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

// 每个模式只认自己那几个参数。传了不管用的参数要当场报错，
// 不能默默忽略——尤其是 -interval：以为限速生效了才敢打真实上游。
func TestRejectsFlagsThatTheModeIgnores(t *testing.T) {
	base := []string{"-direct", "http://x/v1", "-model", "m"}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"吞吐模式不限速", []string{"-mode", "throughput", "-interval", "3s"}, "-interval"},
		{"吞吐模式按时间不按次数", []string{"-mode", "throughput", "-n", "500"}, "-n"},
		{"延迟模式不看时长", []string{"-duration", "10s"}, "-duration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := run(append(base, tc.args...), io.Discard)

			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}
