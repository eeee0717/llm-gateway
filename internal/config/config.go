// Package config 加载并校验网关的 YAML 配置。
// 上游密钥不写在文件里：文件只记环境变量名，加载时再从环境变量读出。
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Config 是网关的全部配置。
type Config struct {
	Listen    string     `yaml:"listen"` // 业务端口的监听地址，默认 :8080
	Admin     Admin      `yaml:"admin"`
	Database  Database   `yaml:"database"`
	Redis     Redis      `yaml:"redis"`
	RateLimit RateLimit  `yaml:"rate_limit"`
	Upstreams []Upstream `yaml:"upstreams"`
	Models    []Model    `yaml:"models"`
}

// Admin 是管理端口的配置。管理接口和业务接口分开监听，方便只对内网开放。
type Admin struct {
	Listen string `yaml:"listen"`  // 管理端口的监听地址，默认 :8081
	KeyEnv string `yaml:"key_env"` // 存放管理员密钥的环境变量名
	Key    string `yaml:"-"`       // 管理员密钥，加载时从 KeyEnv 读出
}

// Database 是 PostgreSQL 的连接配置。连接串里带口令，所以和上游密钥一样只写环境变量名。
type Database struct {
	DSNEnv string `yaml:"dsn_env"` // 存放连接串的环境变量名
	DSN    string `yaml:"-"`       // 连接串，加载时从 DSNEnv 读出
}

// Redis 是 Redis 的连接配置。连接串里可能带口令，所以和数据库一样只写环境变量名，
// 例如 redis://localhost:6379/0。Redis 只做鉴权缓存和限流，连不上时网关照常工作，见 docs/adr/0002。
type Redis struct {
	URLEnv string `yaml:"url_env"` // 存放连接串的环境变量名
	URL    string `yaml:"-"`       // 连接串，加载时从 URLEnv 读出
}

// RateLimit 是限流的配置。额度按 Key 算，每个 Key 可以在库里单独设，见 docs/adr/0004。
type RateLimit struct {
	DefaultRPM int `yaml:"default_rpm"` // Key 没有单独设额度时，每分钟允许的请求数
}

// Upstream 是一个上游。
type Upstream struct {
	Name       string `yaml:"name"`
	BaseURL    string `yaml:"base_url"`     // 例如 https://api.deepseek.com/v1，网关在后面拼上 /chat/completions
	BaseURLEnv string `yaml:"base_url_env"` // 存放上游地址的环境变量名，写了它就不用写 base_url
	KeyEnv     string `yaml:"key_env"`      // 存放上游密钥的环境变量名
	Key        string `yaml:"-"`            // 上游密钥，加载时从 KeyEnv 读出，不写在文件里
}

// Model 是一个模型：名称与上游的模型名一致，固定走一个上游，并带着自己的单价和默认输出上限。
type Model struct {
	Name        string  `yaml:"name"`
	Upstream    string  `yaml:"upstream"`     // 上游的 Name
	InputPrice  float64 `yaml:"input_price"`  // 输入单价，元每百万 prompt token
	OutputPrice float64 `yaml:"output_price"` // 输出单价，元每百万 completion token
	// DefaultMaxTokens 是调用方没指定输出上限时用的默认值。预扣按输出上限算，所以它必须有值。
	DefaultMaxTokens int `yaml:"default_max_tokens"`
}

// Load 读取并校验配置文件。
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cfg Config
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true) // 拼错的字段名直接报错，而不是被悄悄忽略
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.Admin.Listen == "" {
		cfg.Admin.Listen = ":8081"
	}
	if err := cfg.resolve(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
}

// resolve 校验各项配置以及它们之间的引用，并从环境变量读出上游密钥。
func (c *Config) resolve() error {
	var err error
	if c.Database.DSN, err = fromEnv("database", "dsn_env", c.Database.DSNEnv); err != nil {
		return err
	}
	if c.Admin.Key, err = fromEnv("admin", "key_env", c.Admin.KeyEnv); err != nil {
		return err
	}
	if c.Redis.URL, err = fromEnv("redis", "url_env", c.Redis.URLEnv); err != nil {
		return err
	}
	if c.RateLimit.DefaultRPM <= 0 {
		return errors.New("rate_limit: default_rpm must be positive")
	}

	upstreams := make(map[string]bool, len(c.Upstreams))
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		switch {
		case u.Name == "":
			return errors.New("upstream name is required")
		case upstreams[u.Name]:
			return fmt.Errorf("duplicate upstream %q", u.Name)
		}
		upstreams[u.Name] = true

		what := fmt.Sprintf("upstream %q", u.Name)
		// 地址也可以只写变量名：私有的中转地址不一定想写进配置文件。
		if u.BaseURLEnv != "" {
			if u.BaseURL, err = fromEnv(what, "base_url_env", u.BaseURLEnv); err != nil {
				return err
			}
		}
		if !strings.HasPrefix(u.BaseURL, "http://") && !strings.HasPrefix(u.BaseURL, "https://") {
			return fmt.Errorf("upstream %q: base_url must start with http:// or https://", u.Name)
		}
		if u.Key, err = fromEnv(what, "key_env", u.KeyEnv); err != nil {
			return err
		}
	}

	if len(c.Models) == 0 {
		return errors.New("at least one model is required")
	}
	models := make(map[string]bool, len(c.Models))
	for _, m := range c.Models {
		switch {
		case m.Name == "":
			return errors.New("model name is required")
		case models[m.Name]:
			return fmt.Errorf("duplicate model %q", m.Name)
		case !upstreams[m.Upstream]:
			return fmt.Errorf("model %q: unknown upstream %q", m.Name, m.Upstream)
		case m.InputPrice < 0 || m.OutputPrice < 0:
			return fmt.Errorf("model %q: prices must not be negative", m.Name)
		case m.DefaultMaxTokens <= 0:
			return fmt.Errorf("model %q: default_max_tokens must be positive", m.Name)
		}
		models[m.Name] = true
	}
	return nil
}

// fromEnv 读出一个不写在配置文件里的值。what 和 field 只用来指出是哪一处配置出了问题。
func fromEnv(what, field, envName string) (string, error) {
	if envName == "" {
		return "", fmt.Errorf("%s: %s is required", what, field)
	}
	value := os.Getenv(envName)
	if value == "" {
		return "", fmt.Errorf("%s: environment variable %s is not set", what, envName)
	}
	return value, nil
}
