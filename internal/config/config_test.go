package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/eeee0717/llm-gateway/internal/config"
)

const validConfig = `
listen: ":9000"
upstreams:
  - name: deepseek
    base_url: https://api.deepseek.com/v1
    key_env: TEST_DEEPSEEK_KEY
models:
  - name: deepseek-chat
    upstream: deepseek
`

func TestLoadReadsUpstreamKeyFromEnv(t *testing.T) {
	t.Setenv("TEST_DEEPSEEK_KEY", "sk-upstream")

	cfg, err := config.Load(writeConfig(t, validConfig))

	require.NoError(t, err)
	require.Equal(t, &config.Config{
		Listen: ":9000",
		Upstreams: []config.Upstream{{
			Name:    "deepseek",
			BaseURL: "https://api.deepseek.com/v1",
			KeyEnv:  "TEST_DEEPSEEK_KEY",
			Key:     "sk-upstream",
		}},
		Models: []config.Model{{Name: "deepseek-chat", Upstream: "deepseek"}},
	}, cfg)
}

func TestLoadDefaultsListenAddress(t *testing.T) {
	t.Setenv("TEST_DEEPSEEK_KEY", "sk-upstream")

	cfg, err := config.Load(writeConfig(t, strings.Replace(validConfig, `listen: ":9000"`, "", 1)))

	require.NoError(t, err)
	require.Equal(t, ":8080", cfg.Listen)
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	t.Setenv("TEST_DEEPSEEK_KEY", "sk-upstream")

	for _, tc := range []struct {
		name    string
		content string
		wantErr string // 错误信息里要指出出错的地方
	}{
		{"unknown field", validConfig + "timeout: 3s\n", "timeout"},
		{"key env not set", strings.Replace(validConfig, "TEST_DEEPSEEK_KEY", "TEST_UNSET_KEY", 1), "TEST_UNSET_KEY"},
		{"base url without scheme", strings.Replace(validConfig, "https://", "", 1), "base_url"},
		{"unknown upstream", strings.Replace(validConfig, "upstream: deepseek", "upstream: openai", 1), `"openai"`},
		{"duplicate model", validConfig + "  - name: deepseek-chat\n    upstream: deepseek\n", `"deepseek-chat"`},
		{"no models", strings.Split(validConfig, "models:")[0], "model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(writeConfig(t, tc.content))
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}
