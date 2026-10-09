// Reads the YAML settings and checks them before the server starts.
// Fills in missing defaults and rejects invalid settings. Start with loadConfig.

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

// validateCircuitBreaker checks how many failures are allowed and how long to wait.
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

// loadConfig opens the settings file and passes it to decodeConfig.
func loadConfig(path string) (Config, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	return decodeConfig(f)
}

// decodeConfig converts YAML into Go settings and checks them.
// Reject unknown setting names and files containing more than one YAML document.
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

// validate checks the gateway settings, then each route.
// Reject two routes that use the same path and HTTP method.
func (c *Config) validate() ([]string, error) {
	if err := c.Gateway.validate(); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range c.Routes {
		label := fmt.Sprintf("routes[%d]", i)
		if err := c.Routes[i].validate(label, c.Gateway.GlobalTimeout, seen); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// validate fills in missing gateway defaults and checks their values.
func (g *GatewayConfig) validate() error {
	if g.Port == 0 {
		g.Port = 8080
	}
	if g.Port < 1 || g.Port > 65535 {
		return fmt.Errorf("gateway.port must be between 1 and 65535")
	}
	if g.GlobalTimeout == "" {
		g.GlobalTimeout = "30s"
	}
	if _, err := duration(g.GlobalTimeout); err != nil {
		return fmt.Errorf("gateway.global_timeout: %w", err)
	}
	if err := validateLimit(g.GlobalRateLimit); err != nil {
		return fmt.Errorf("gateway.global_rate_limit: %w", err)
	}
	return nil
}

// validate checks this route's path, methods, backend, and optional features.
// Error messages name the route; seen tracks path/method pairs across all routes.
func (r *RouteConfig) validate(label, defaultTimeout string, seen map[string]bool) error {
	if err := r.validateMatch(label, seen); err != nil {
		return err
	}
	if err := r.Upstream.validateTimeout(label, defaultTimeout); err != nil {
		return err
	}
	if err := validateLimit(r.RateLimit); err != nil {
		return fmt.Errorf("%s.rate_limit: %w", label, err)
	}
	if err := r.Upstream.validateDestinations(label); err != nil {
		return err
	}
	if err := validateHealthCheck(r.HealthCheck); err != nil {
		return fmt.Errorf("%s.health_check: %w", label, err)
	}
	if err := validateAuth(r.Auth, label); err != nil {
		return err
	}
	if err := validateTransforms(*r); err != nil {
		return fmt.Errorf("%s.transform: %w", label, err)
	}
	if err := validateCircuitBreaker(r.CircuitBreaker); err != nil {
		return fmt.Errorf("%s.circuit_breaker: %w", label, err)
	}
	if err := validateRetry(r.Retry); err != nil {
		return fmt.Errorf("%s.retry: %w", label, err)
	}
	return nil
}

// validateMatch checks the path and methods and removes trailing slashes.
// Two routes can share a path if they accept different HTTP methods.
func (r *RouteConfig) validateMatch(label string, seen map[string]bool) error {
	if !strings.HasPrefix(r.Path, "/") || strings.ContainsAny(r.Path, "?#\r\n") {
		return fmt.Errorf("%s.path must be an absolute URL path without query or fragment", label)
	}
	if ambiguousPath(&url.URL{Path: r.Path}) {
		return fmt.Errorf("%s.path contains ambiguous separators or dot segments", label)
	}
	r.Path = strings.TrimRight(r.Path, "/")
	if r.Path == "" {
		r.Path = "/"
	}
	if len(r.Methods) == 0 {
		return fmt.Errorf("%s.methods must not be empty", label)
	}
	for _, method := range r.Methods {
		if !validToken(method) || method != strings.ToUpper(method) {
			return fmt.Errorf("%s.methods must contain uppercase HTTP method tokens", label)
		}
		key := r.Path + " " + method
		if seen[key] {
			return fmt.Errorf("duplicate route/method %s", key)
		}
		seen[key] = true
	}
	return nil
}

// validateTimeout uses the gateway's time limit when the route has none.
func (u *UpstreamConfig) validateTimeout(label, defaultTimeout string) error {
	if u.Timeout == "" {
		u.Timeout = defaultTimeout
	}
	if _, err := duration(u.Timeout); err != nil {
		return fmt.Errorf("%s.upstream.timeout: %w", label, err)
	}
	return nil
}

// validateDestinations checks backend addresses and how the gateway chooses between them.
func (u *UpstreamConfig) validateDestinations(label string) error {
	if (u.URL == "") == (len(u.Targets) == 0) {
		return fmt.Errorf("%s.upstream requires exactly one of url or targets", label)
	}
	if u.URL != "" {
		if err := validateURL(u.URL); err != nil {
			return fmt.Errorf("%s.upstream.url: %w", label, err)
		}
	}
	if u.Balance == "" {
		u.Balance = "round_robin"
	}
	if u.Balance != "round_robin" && u.Balance != "weighted_round_robin" {
		return fmt.Errorf("%s.upstream.balance is unsupported", label)
	}
	for j, target := range u.Targets {
		if err := validateURL(target.URL); err != nil {
			return fmt.Errorf("%s.upstream.targets[%d]: %w", label, j, err)
		}
		if u.Balance == "weighted_round_robin" && (target.Weight <= 0 || target.Weight > 1000000) {
			return fmt.Errorf("%s.upstream.targets[%d].weight must be between 1 and 1000000", label, j)
		}
	}
	return nil
}

// validateAuth checks optional API-key settings without exposing key values in errors.
func validateAuth(c *AuthConfig, label string) error {
	if c == nil {
		return nil
	}
	if c.Type != "api_key" || !validToken(c.Header) || len(c.Keys) == 0 {
		return fmt.Errorf("%s.auth requires type api_key, a valid header, and nonempty keys", label)
	}
	for _, key := range c.Keys {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("%s.auth.keys must not contain empty values", label)
		}
	}
	return nil
}

// validateLimit checks the request limit, time period, counting method, and client grouping.
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

// duration reads a positive time value, such as 500ms or 30s.
func duration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("must be a positive duration (for example 500ms, 5s, or 1m)")
	}
	return d, nil
}

// validateURL requires an HTTP(S) backend address with no username, password, or # fragment.
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
