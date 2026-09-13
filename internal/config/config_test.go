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
admin:
  listen: ":9001"
  key_env: TEST_ADMIN_KEY
database:
  dsn_env: TEST_DATABASE_DSN
redis:
  url_env: TEST_REDIS_URL
rate_limit:
  default_rpm: 60
upstreams:
  - name: deepseek
    base_url: https://api.deepseek.com/v1
    key_env: TEST_DEEPSEEK_KEY
models:
  - name: deepseek-chat
    upstream: deepseek
    input_price: 2
    output_price: 3
    default_max_tokens: 4096
`

func TestLoadReadsSecretsFromEnv(t *testing.T) {
	setenv(t)

	cfg, err := config.Load(writeConfig(t, validConfig))

	require.NoError(t, err)
	require.Equal(t, &config.Config{
		Listen:    ":9000",
		Admin:     config.Admin{Listen: ":9001", KeyEnv: "TEST_ADMIN_KEY", Key: "admin-secret"},
		Database:  config.Database{DSNEnv: "TEST_DATABASE_DSN", DSN: "postgres://localhost/test"},
		Redis:     config.Redis{URLEnv: "TEST_REDIS_URL", URL: "redis://localhost:6379/0"},
		RateLimit: config.RateLimit{DefaultRPM: 60},
		Upstreams: []config.Upstream{{
			Name:    "deepseek",
			BaseURL: "https://api.deepseek.com/v1",
			KeyEnv:  "TEST_DEEPSEEK_KEY",
			Key:     "sk-upstream",
		}},
		Models: []config.Model{{
			Name:             "deepseek-chat",
			Upstream:         "deepseek",
			InputPrice:       2,
			OutputPrice:      3,
			DefaultMaxTokens: 4096,
		}},
	}, cfg)
}

func TestLoadDefaultsListenAddresses(t *testing.T) {
	setenv(t)
	noListen := strings.NewReplacer(`listen: ":9000"`, "", `  listen: ":9001"`, "").Replace(validConfig)

	cfg, err := config.Load(writeConfig(t, noListen))

	require.NoError(t, err)
	require.Equal(t, ":8080", cfg.Listen)
	require.Equal(t, ":8081", cfg.Admin.Listen)
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	setenv(t)

	for _, tc := range []struct {
		name    string
		content string
		wantErr string // 错误信息里要指出出错的地方
	}{
		{"unknown field", validConfig + "timeout: 3s\n", "timeout"},
		{"key env not set", strings.Replace(validConfig, "TEST_DEEPSEEK_KEY", "TEST_UNSET_KEY", 1), "TEST_UNSET_KEY"},
		{"dsn env not set", strings.Replace(validConfig, "TEST_DATABASE_DSN", "TEST_UNSET_DSN", 1), "TEST_UNSET_DSN"},
		{"no database", strings.Replace(validConfig, "  dsn_env: TEST_DATABASE_DSN\n", "", 1), "dsn_env"},
		{"admin key env not set", strings.Replace(validConfig, "TEST_ADMIN_KEY", "TEST_UNSET_ADMIN_KEY", 1), "TEST_UNSET_ADMIN_KEY"},
		{"redis url env not set", strings.Replace(validConfig, "TEST_REDIS_URL", "TEST_UNSET_REDIS_URL", 1), "TEST_UNSET_REDIS_URL"},
		{"no redis", strings.Replace(validConfig, "  url_env: TEST_REDIS_URL\n", "", 1), "url_env"},
		{"rate limit not positive", strings.Replace(validConfig, "default_rpm: 60", "default_rpm: 0", 1), "default_rpm"},
		{"base url without scheme", strings.Replace(validConfig, "https://", "", 1), "base_url"},
		{"unknown upstream", strings.Replace(validConfig, "upstream: deepseek", "upstream: openai", 1), `"openai"`},
		{"duplicate model", validConfig + "  - name: deepseek-chat\n    upstream: deepseek\n", `"deepseek-chat"`},
		{"model without output limit", strings.Replace(validConfig, "    default_max_tokens: 4096\n", "", 1), "default_max_tokens"},
		{"no models", strings.Split(validConfig, "models:")[0], "model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(writeConfig(t, tc.content))
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// setenv 设置示例配置引用的环境变量。
func setenv(t *testing.T) {
	t.Helper()
	t.Setenv("TEST_DEEPSEEK_KEY", "sk-upstream")
	t.Setenv("TEST_DATABASE_DSN", "postgres://localhost/test")
	t.Setenv("TEST_ADMIN_KEY", "admin-secret")
	t.Setenv("TEST_REDIS_URL", "redis://localhost:6379/0")
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}
