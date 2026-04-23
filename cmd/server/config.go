package main

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Global struct {
		RedisAddr string `yaml:"redis_addr"`
		DBDsn     string `yaml:"db_dsn"`
		WebSocket struct {
			Enabled         bool   `yaml:"enabled"`
			ListenAddr      string `yaml:"listen_addr"`
			Path            string `yaml:"path"`
			AuthHeader      string `yaml:"auth_header"`
			AuthToken       string `yaml:"auth_token"`
			EventBufferSize int    `yaml:"event_buffer_size"`
			WriteTimeoutMs  int    `yaml:"write_timeout_ms"`
		} `yaml:"websocket"`
	} `yaml:"global"`
	Plugins []struct {
		Name    string      `yaml:"name"`
		Enabled bool        `yaml:"enabled"`
		Configs []yaml.Node `yaml:"configs"` // 支持多个配置项
		Config  yaml.Node   `yaml:"config"`  // 兼容旧格式
	} `yaml:"plugins"`
}

func loadConfig() *Config {
	data, err := os.ReadFile("config.yaml")
	if err != nil {
		panic(err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		panic(err)
	}
	return &cfg
}
