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

func TestConfigDeferredFeaturesWarn(t *testing.T) {
	input := minimalConfig + `    health_check: {path: /healthz, interval: 1s}
    retry: {attempts: 3}
    request_transform: {headers: {add: {X-Test: yes}}}
    response_transform: {body: {envelope: {data: $body}}}
    circuit_breaker: {threshold: 5}
`
	_, warnings, err := decodeConfig(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 5 {
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
