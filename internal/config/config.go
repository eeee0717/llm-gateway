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
	Upstreams []Upstream `yaml:"upstreams"`
	Models    []Model    `yaml:"models"`
}

// Upstream 是一个上游。
type Upstream struct {
	Name    string `yaml:"name"`
	BaseURL string `yaml:"base_url"` // 例如 https://api.deepseek.com/v1，网关在后面拼上 /chat/completions
	KeyEnv  string `yaml:"key_env"`  // 存放上游密钥的环境变量名
	Key     string `yaml:"-"`        // 上游密钥，加载时从 KeyEnv 读出，不写在文件里
}

// Model 是一个模型：名称与上游的模型名一致，固定走一个上游。
type Model struct {
	Name     string `yaml:"name"`
	Upstream string `yaml:"upstream"` // 上游的 Name
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
	if err := cfg.resolve(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
}

// resolve 校验各项配置以及它们之间的引用，并从环境变量读出上游密钥。
func (c *Config) resolve() error {
	upstreams := make(map[string]bool, len(c.Upstreams))
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		switch {
		case u.Name == "":
			return errors.New("upstream name is required")
		case upstreams[u.Name]:
			return fmt.Errorf("duplicate upstream %q", u.Name)
		case !strings.HasPrefix(u.BaseURL, "http://") && !strings.HasPrefix(u.BaseURL, "https://"):
			return fmt.Errorf("upstream %q: base_url must start with http:// or https://", u.Name)
		case u.KeyEnv == "":
			return fmt.Errorf("upstream %q: key_env is required", u.Name)
		}
		upstreams[u.Name] = true
		if u.Key = os.Getenv(u.KeyEnv); u.Key == "" {
			return fmt.Errorf("upstream %q: environment variable %s is not set", u.Name, u.KeyEnv)
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
		}
		models[m.Name] = true
	}
	return nil
}
