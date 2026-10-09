package main

import (
	"strings"
	"testing"
)

const minimalConfig = `gateway:
  port: 8080
routes:
  - path: /different
    methods: [GET, POST]
    upstream:
      url: http://localhost:3001
`

func TestConfigDefaultsAndAlternateValues(t *testing.T) {
	c, warnings, err := decodeConfig(strings.NewReader(strings.ReplaceAll(minimalConfig, "8080", "9191")))
	if err != nil {
		t.Fatal(err)
	}
	if c.Gateway.Port != 9191 || c.Routes[0].Path != "/different" || c.Routes[0].Upstream.Timeout != "30s" || len(warnings) != 0 {
		t.Fatalf("unexpected config: %+v warnings=%v", c, warnings)
	}
	c, _, err = decodeConfig(strings.NewReader("gateway: {}\nroutes: []\n"))
	if err != nil || c.Gateway.Port != 8080 {
		t.Fatalf("default port: %+v %v", c, err)
	}
}

func TestConfigValidationErrors(t *testing.T) {
	const durationError = "must be a positive duration (for example 500ms, 5s, or 1m)"
	for _, tc := range []struct {
		name, input, want string
	}{
		{"gateway timeout", strings.Replace(minimalConfig, "port: 8080", "global_timeout: invalid", 1), "gateway.global_timeout: " + durationError},
		{"gateway limit", strings.Replace(minimalConfig, "port: 8080", "global_rate_limit: {requests: 0}", 1), "gateway.global_rate_limit: requests must be positive"},
		{"path", strings.Replace(minimalConfig, "/different", "relative", 1), "routes[0].path must be an absolute URL path without query or fragment"},
		{"ambiguous path", strings.Replace(minimalConfig, "/different", "/a/../b", 1), "routes[0].path contains ambiguous separators or dot segments"},
		{"empty methods", strings.Replace(minimalConfig, "[GET, POST]", "[]", 1), "routes[0].methods must not be empty"},
		{"method token", strings.Replace(minimalConfig, "[GET, POST]", "['G ET']", 1), "routes[0].methods must contain uppercase HTTP method tokens"},
		{"duplicate method", strings.Replace(minimalConfig, "[GET, POST]", "[GET, GET]", 1), "duplicate route/method /different GET"},
		{"upstream timeout before limit", minimalConfig + "      timeout: invalid\n    rate_limit: {requests: 0}\n", "routes[0].upstream.timeout: " + durationError},
		{"limit before destination", strings.Replace(minimalConfig, "http://localhost:3001", "invalid", 1) + "    rate_limit: {requests: 0}\n", "routes[0].rate_limit: requests must be positive"},
		{"missing upstream", strings.Replace(minimalConfig, "url: http://localhost:3001", "url: ''", 1), "routes[0].upstream requires exactly one of url or targets"},
		{"destination", strings.Replace(minimalConfig, "http://localhost:3001", "file:///tmp/backend", 1), "routes[0].upstream.url: must be an http(s) URL without credentials or fragment"},
		{"balance", minimalConfig + "      balance: random\n", "routes[0].upstream.balance is unsupported"},
		{"target", strings.Replace(minimalConfig, "url: http://localhost:3001", "targets: [{url: invalid}]", 1), "routes[0].upstream.targets[0]: must be an http(s) URL without credentials or fragment"},
		{"weight", strings.Replace(minimalConfig, "url: http://localhost:3001", "targets: [{url: 'http://localhost:1', weight: 0}]\n      balance: weighted_round_robin", 1), "routes[0].upstream.targets[0].weight must be between 1 and 1000000"},
		{"auth", minimalConfig + "    auth: {type: jwt, header: X-Key, keys: [abc]}\n", "routes[0].auth requires type api_key, a valid header, and nonempty keys"},
		{"blank key", minimalConfig + "    auth: {type: api_key, header: X-Key, keys: [' ']}\n", "routes[0].auth.keys must not contain empty values"},
		{"health", minimalConfig + "    health_check: {path: /healthz, interval: invalid}\n", "routes[0].health_check: interval: " + durationError},
		{"transform", minimalConfig + "    request_transform: {headers: {add: {Connection: close}}}\n", `routes[0].transform: invalid, duplicate or protected header "Connection"`},
		{"breaker", minimalConfig + "    circuit_breaker: {threshold: 0}\n", "routes[0].circuit_breaker: threshold must be between 1 and 1000000"},
		{"retry", minimalConfig + "    retry: {attempts: 0}\n", "routes[0].retry: attempts must be between 1 and 100 (including the first)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, warnings, err := decodeConfig(strings.NewReader(tc.input))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if len(warnings) != 0 {
				t.Fatalf("unexpected warnings: %v", warnings)
			}
		})
	}
}

func TestConfigNormalizedRouteMethods(t *testing.T) {
	input := `gateway:
  global_timeout: 7s
routes:
  - path: /shared/
    methods: [GET]
    upstream: {url: 'http://localhost:1'}
  - path: /shared
    methods: [POST]
    upstream: {url: 'http://localhost:2', timeout: 2s}
  - path: /
    methods: [GET]
    upstream: {url: 'http://localhost:3'}
`
	c, _, err := decodeConfig(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if c.Gateway.Port != 8080 || c.Routes[0].Path != "/shared" || c.Routes[2].Path != "/" {
		t.Fatalf("unexpected normalized configuration: %+v", c)
	}
	for i, want := range []string{"7s", "2s", "7s"} {
		if c.Routes[i].Upstream.Timeout != want || c.Routes[i].Upstream.Balance != "round_robin" {
			t.Fatalf("route %d: %+v", i, c.Routes[i].Upstream)
		}
	}
	_, _, err = decodeConfig(strings.NewReader(strings.Replace(input, "methods: [POST]", "methods: [GET]", 1)))
	if err == nil || err.Error() != "duplicate route/method /shared GET" {
		t.Fatalf("expected normalized route conflict, got %v", err)
	}
}

func TestConfigInvalid(t *testing.T) {
	cases := map[string]string{
		"malformed":           "gateway: [",
		"unknown field":       minimalConfig + "surprise: true\n",
		"multiple documents":  minimalConfig + "---\ngateway: {}\n",
		"port":                strings.ReplaceAll(minimalConfig, "8080", "70000"),
		"path":                strings.ReplaceAll(minimalConfig, "/different", "relative"),
		"method":              strings.ReplaceAll(minimalConfig, "[GET, POST]", "[get]"),
		"empty methods":       strings.ReplaceAll(minimalConfig, "[GET, POST]", "[]"),
		"duplicate method":    strings.ReplaceAll(minimalConfig, "[GET, POST]", "[GET, GET]"),
		"upstream scheme":     strings.ReplaceAll(minimalConfig, "http://localhost:3001", "file:///etc/passwd"),
		"credentials":         strings.ReplaceAll(minimalConfig, "http://localhost:3001", "http://a:b@localhost:3001"),
		"missing upstream":    strings.ReplaceAll(minimalConfig, "url: http://localhost:3001", "url: ''"),
		"bad timeout":         minimalConfig + "      timeout: 0s\n",
		"bad balance":         minimalConfig + "      balance: random\n",
		"bad auth":            minimalConfig + "    auth: {type: jwt, header: X-Key, keys: [abc]}\n",
		"empty auth key":      minimalConfig + "    auth: {type: api_key, header: X-Key, keys: ['']}\n",
		"invalid rate limit":  minimalConfig + "    rate_limit: {requests: 0, window: 1s, strategy: fixed_window, per: ip}\n",
		"invalid rate window": minimalConfig + "    rate_limit: {requests: 1, window: bad, strategy: fixed_window, per: ip}\n",
		"invalid strategy":    minimalConfig + "    rate_limit: {requests: 1, window: 1s, strategy: token, per: ip}\n",
		"invalid key":         minimalConfig + "    rate_limit: {requests: 1, window: 1s, strategy: fixed_window, per: user}\n",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := decodeConfig(strings.NewReader(input)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestConfigAllFeaturesValidate(t *testing.T) {
	input := minimalConfig + `    health_check: {path: /healthz, interval: 1s}
    retry: {attempts: 3, backoff: fixed, initial_delay: 1ms, on: [503]}
    request_transform: {headers: {add: {X-Test: yes}}}
    response_transform: {body: {envelope: {data: $body}}}
    circuit_breaker: {threshold: 5, window: "60s", cooldown: "30s"}
`
	_, warnings, err := decodeConfig(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings: %v", warnings)
	}
}

func TestTargetValidation(t *testing.T) {
	for _, tc := range []struct {
		name, upstream string
		valid          bool
	}{
		{"round robin", "targets: [{url: 'http://localhost:1'}]", true},
		{"weighted", "targets: [{url: 'http://localhost:1', weight: 3}]\n      balance: weighted_round_robin", true},
		{"missing weight", "targets: [{url: 'http://localhost:1'}]\n      balance: weighted_round_robin", false},
		{"two upstream forms", "url: http://localhost:1\n      targets: [{url: 'http://localhost:2'}]", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := decodeConfig(strings.NewReader(strings.Replace(minimalConfig, "url: http://localhost:3001", tc.upstream, 1)))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestProvidedConfiguration(t *testing.T) {
	c, warnings, err := loadConfig("gateway.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Routes) != 5 || len(warnings) != 0 || c.Routes[1].Upstream.Timeout != "5s" || c.Routes[2].Upstream.Timeout != "10s" {
		t.Fatalf("provided config not represented correctly: routes=%d warnings=%v", len(c.Routes), warnings)
	}
	if _, warnings, err := loadConfig("examples/demo.yaml"); err != nil || len(warnings) != 0 {
		t.Fatalf("demo config: %v %v", warnings, err)
	}
	if _, _, err := loadConfig("does-not-exist.yaml"); err == nil {
		t.Fatal("missing file accepted")
	}
}
