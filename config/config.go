package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

type Config struct {
	Mode         string `json:"mode"`
	StartOnLogin bool   `json:"startOnLogin"`
	Port         int    `json:"port"`
	baseDir      string
}

func defaultConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine config directory: %w", err)
	}
	return base, nil
}

func Dir() (string, error) {
	base, err := defaultConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "htmlclay"), nil
}

func DirFrom(baseDir string) string {
	return filepath.Join(baseDir, "htmlclay")
}

func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

func EnsureDir() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	return os.MkdirAll(dir, 0755)
}

func Load() (*Config, error) {
	base, err := defaultConfigDir()
	if err != nil {
		return nil, err
	}
	return LoadFrom(base)
}

func LoadFrom(baseDir string) (*Config, error) {
	cfg := &Config{
		Mode:         "app",
		StartOnLogin: false,
		Port:         0,
		baseDir:      baseDir,
	}

	path := filepath.Join(DirFrom(baseDir), "config.json")

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) Save() error {
	dir := DirFrom(c.baseDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dir, "config.json")
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func (c *Config) ResolvePort() (net.Listener, error) {
	if c.Port != 0 {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", c.Port))
		if err == nil {
			return ln, nil
		}
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	c.Port = ln.Addr().(*net.TCPAddr).Port
	if err := c.Save(); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}
