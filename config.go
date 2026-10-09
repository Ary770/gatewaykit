// Startup configuration boundary: decode YAML into typed settings, fill defaults,
// and reject invalid routes or policies before accepting traffic. Start at loadConfig.

package main

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Gateway GatewayConfig `yaml:"gateway"`
	Routes  []RouteConfig `yaml:"routes"`
}

type GatewayConfig struct {
	Port            int              `yaml:"port"`
	GlobalTimeout   string           `yaml:"global_timeout"`
	GlobalRateLimit *RateLimitConfig `yaml:"global_rate_limit"`
}

type RouteConfig struct {
	HealthCheck       *HealthCheckConfig       `yaml:"health_check"`
	Path              string                   `yaml:"path"`
	Methods           []string                 `yaml:"methods"`
	StripPrefix       bool                     `yaml:"strip_prefix"`
	Upstream          UpstreamConfig           `yaml:"upstream"`
	RateLimit         *RateLimitConfig         `yaml:"rate_limit"`
	Auth              *AuthConfig              `yaml:"auth"`
	Retry             *RetryConfig             `yaml:"retry"`
	RequestTransform  *RequestTransformConfig  `yaml:"request_transform"`
	ResponseTransform *ResponseTransformConfig `yaml:"response_transform"`
	CircuitBreaker    *CircuitBreakerConfig    `yaml:"circuit_breaker"`
}

type HealthCheckConfig struct {
	Path               string `yaml:"path"`
	Interval           string `yaml:"interval"`
	UnhealthyThreshold int    `yaml:"unhealthy_threshold"`
}

type CircuitBreakerConfig struct {
	Threshold int    `yaml:"threshold"`
	Window    string `yaml:"window"`
	Cooldown  string `yaml:"cooldown"`
}

// validateCircuitBreaker bounds the failure threshold and checks the two time periods.
func validateCircuitBreaker(c *CircuitBreakerConfig) error {
	if c == nil {
		return nil
	}
	if c.Threshold < 1 || c.Threshold > 1000000 {
		return fmt.Errorf("threshold must be between 1 and 1000000")
	}
	if _, err := duration(c.Window); err != nil {
		return fmt.Errorf("window: %w", err)
	}
	if _, err := duration(c.Cooldown); err != nil {
		return fmt.Errorf("cooldown: %w", err)
	}
	return nil
}

type UpstreamConfig struct {
	Timeout string         `yaml:"timeout"`
	URL     string         `yaml:"url"`
	Targets []TargetConfig `yaml:"targets"`
	Balance string         `yaml:"balance"`
}

type TargetConfig struct {
	URL    string `yaml:"url"`
	Weight int    `yaml:"weight"`
}

type RateLimitConfig struct {
	Requests int    `yaml:"requests"`
	Window   string `yaml:"window"`
	Strategy string `yaml:"strategy"`
	Per      string `yaml:"per"`
}

type AuthConfig struct {
	Type   string   `yaml:"type"`
	Header string   `yaml:"header"`
	Keys   []string `yaml:"keys"`
}

// loadConfig opens the selected file and delegates parsing and validation to decodeConfig.
func loadConfig(path string) (Config, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	return decodeConfig(f)
}

// decodeConfig accepts exactly one YAML document, rejects unknown fields, and validates it.
func decodeConfig(r io.Reader) (Config, []string, error) {
	var c Config
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)
	if err := decoder.Decode(&c); err != nil {
		return c, nil, fmt.Errorf("parse config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return c, nil, fmt.Errorf("config must contain exactly one YAML document")
	}
	warnings, err := c.validate()
	return c, warnings, err
}

// validate resolves defaults in place and checks every route, including overlapping
// method/path definitions. Later request handling can rely on these validated settings.
func (c *Config) validate() ([]string, error) {
	if c.Gateway.Port == 0 {
		c.Gateway.Port = 8080
	}
	if c.Gateway.Port < 1 || c.Gateway.Port > 65535 {
		return nil, fmt.Errorf("gateway.port must be between 1 and 65535")
	}
	if c.Gateway.GlobalTimeout == "" {
		c.Gateway.GlobalTimeout = "30s"
	}
	if _, err := duration(c.Gateway.GlobalTimeout); err != nil {
		return nil, fmt.Errorf("gateway.global_timeout: %w", err)
	}
	if err := validateLimit(c.Gateway.GlobalRateLimit); err != nil {
		return nil, fmt.Errorf("gateway.global_rate_limit: %w", err)
	}
	seen := map[string]bool{}
	for i := range c.Routes {
		r := &c.Routes[i]
		label := fmt.Sprintf("routes[%d]", i)
		if !strings.HasPrefix(r.Path, "/") || strings.ContainsAny(r.Path, "?#\r\n") {
			return nil, fmt.Errorf("%s.path must be an absolute URL path without query or fragment", label)
		}
		if ambiguousPath(&url.URL{Path: r.Path}) {
			return nil, fmt.Errorf("%s.path contains ambiguous separators or dot segments", label)
		}
		r.Path = strings.TrimRight(r.Path, "/")
		if r.Path == "" {
			r.Path = "/"
		}
		if len(r.Methods) == 0 {
			return nil, fmt.Errorf("%s.methods must not be empty", label)
		}
		for _, method := range r.Methods {
			if !validToken(method) || method != strings.ToUpper(method) {
				return nil, fmt.Errorf("%s.methods must contain uppercase HTTP method tokens", label)
			}
			key := r.Path + " " + method
			if seen[key] {
				return nil, fmt.Errorf("duplicate route/method %s", key)
			}
			seen[key] = true
		}
		if r.Upstream.Timeout == "" {
			r.Upstream.Timeout = c.Gateway.GlobalTimeout
		}
		if _, err := duration(r.Upstream.Timeout); err != nil {
			return nil, fmt.Errorf("%s.upstream.timeout: %w", label, err)
		}
		if err := validateLimit(r.RateLimit); err != nil {
			return nil, fmt.Errorf("%s.rate_limit: %w", label, err)
		}
		u := &r.Upstream
		if (u.URL == "") == (len(u.Targets) == 0) {
			return nil, fmt.Errorf("%s.upstream requires exactly one of url or targets", label)
		}
		if u.URL != "" {
			if err := validateURL(u.URL); err != nil {
				return nil, fmt.Errorf("%s.upstream.url: %w", label, err)
			}
		}
		if u.Balance == "" {
			u.Balance = "round_robin"
		}
		if u.Balance != "round_robin" && u.Balance != "weighted_round_robin" {
			return nil, fmt.Errorf("%s.upstream.balance is unsupported", label)
		}
		for j, target := range u.Targets {
			if err := validateURL(target.URL); err != nil {
				return nil, fmt.Errorf("%s.upstream.targets[%d]: %w", label, j, err)
			}
			if u.Balance == "weighted_round_robin" && (target.Weight <= 0 || target.Weight > 1000000) {
				return nil, fmt.Errorf("%s.upstream.targets[%d].weight must be between 1 and 1000000", label, j)
			}
		}
		if err := validateHealthCheck(r.HealthCheck); err != nil {
			return nil, fmt.Errorf("%s.health_check: %w", label, err)
		}
		if r.Auth != nil {
			if r.Auth.Type != "api_key" || !validToken(r.Auth.Header) || len(r.Auth.Keys) == 0 {
				return nil, fmt.Errorf("%s.auth requires type api_key, a valid header, and nonempty keys", label)
			}
			for _, key := range r.Auth.Keys {
				if strings.TrimSpace(key) == "" {
					return nil, fmt.Errorf("%s.auth.keys must not contain empty values", label)
				}
			}
		}
		if err := validateTransforms(*r); err != nil {
			return nil, fmt.Errorf("%s.transform: %w", label, err)
		}
		if err := validateCircuitBreaker(r.CircuitBreaker); err != nil {
			return nil, fmt.Errorf("%s.circuit_breaker: %w", label, err)
		}
		if err := validateRetry(r.Retry); err != nil {
			return nil, fmt.Errorf("%s.retry: %w", label, err)
		}

	}
	return nil, nil
}

// validateLimit checks quota size, duration, counting strategy, and client grouping.
func validateLimit(c *RateLimitConfig) error {
	if c == nil {
		return nil
	}
	if c.Requests <= 0 {
		return fmt.Errorf("requests must be positive")
	}
	if _, err := duration(c.Window); err != nil {
		return fmt.Errorf("window: %w", err)
	}
	if c.Strategy != "fixed_window" && c.Strategy != "sliding_window" {
		return fmt.Errorf("strategy must be fixed_window or sliding_window")
	}
	if c.Per != "ip" && c.Per != "global" {
		return fmt.Errorf("per must be ip or global")
	}
	return nil
}

// duration parses a strictly positive Go duration, such as 500ms or 30s.
func duration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("must be a positive duration (for example 500ms, 5s, or 1m)")
	}
	return d, nil
}

// validateURL restricts destinations to HTTP(S) URLs without credentials or fragments.
func validateURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("must be an http(s) URL without credentials or fragment")
	}
	return nil
}

func validToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r) {
			continue
		}
		return false
	}
	return true
}
