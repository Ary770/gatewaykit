// Configured representation changes: edit headers, map request JSON fields, and
// wrap the final backend JSON response in an envelope. Header-only rules stream;
// body rules buffer within 1 MiB. Start at transformRequest and transformResponse.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

const transformBodyLimit = 1 << 20

type TransformHeaders struct {
	Add    map[string]string `yaml:"add"`
	Remove []string          `yaml:"remove"`
}
type RequestTransformConfig struct {
	Headers TransformHeaders      `yaml:"headers"`
	Body    *RequestTransformBody `yaml:"body"`
}
type RequestTransformBody struct {
	Mapping map[string]string `yaml:"mapping"`
}
type ResponseTransformConfig struct {
	Headers TransformHeaders       `yaml:"headers"`
	Body    *ResponseTransformBody `yaml:"body"`
}
type ResponseTransformBody struct {
	Envelope map[string]any `yaml:"envelope"`
}
type transformValues struct{ requestTime, responseTime, route string }

// transformExpression resolves the supported metadata/body tokens; it never evaluates code.
func transformExpression(s string, v transformValues, body any) any {
	switch s {
	case "$request_time":
		return v.requestTime
	case "$response_time":
		return v.responseTime
	case "$route_path":
		return v.route
	case "$body":
		return body
	}
	return strings.TrimPrefix(s, "$literal:")
}
func validateExpression(s string, bodyAllowed bool) error {
	if !strings.HasPrefix(s, "$") || strings.HasPrefix(s, "$literal:") {
		return nil
	}
	if s == "$request_time" || s == "$response_time" || s == "$route_path" || bodyAllowed && s == "$body" {
		return nil
	}
	return fmt.Errorf("unsupported expression %q", s)
}
func protectedTransformHeader(s string) bool {
	s = strings.ToLower(s)
	switch s {
	case "host", "content-length", "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "forwarded":
		return true
	}
	return strings.HasPrefix(s, "x-forwarded-")
}
func validateTransformHeaders(h TransformHeaders) error {
	for _, name := range h.Remove {
		if !validToken(name) || protectedTransformHeader(name) {
			return fmt.Errorf("invalid or protected header %q", name)
		}
	}
	seen := map[string]bool{}
	for name, value := range h.Add {
		if !validToken(name) || protectedTransformHeader(name) || seen[strings.ToLower(name)] {
			return fmt.Errorf("invalid, duplicate or protected header %q", name)
		}
		seen[strings.ToLower(name)] = true
		for _, c := range value {
			if c == 127 || c < 32 && c != '\t' {
				return fmt.Errorf("invalid header value for %s", name)
			}
		}
		if err := validateExpression(value, false); err != nil {
			return err
		}
	}
	return nil
}
func validFieldPath(s string) bool {
	for _, part := range strings.Split(s, ".") {
		if part == "" || strings.ContainsAny(part, "$[] \t\r\n") {
			return false
		}
	}
	return true
}
func validateEnvelope(v any) error {
	switch x := v.(type) {
	case string:
		return validateExpression(x, true)
	case map[string]any:
		for _, v := range x {
			if err := validateEnvelope(v); err != nil {
				return err
			}
		}
	case []any:
		for _, v := range x {
			if err := validateEnvelope(v); err != nil {
				return err
			}
		}
	case nil, bool, int, int64, uint64, float64:
	default:
		return fmt.Errorf("unsupported envelope value")
	}
	return nil
}

// validateTransforms checks header rules, expressions, and conflicting mapping paths
// at startup so invalid rewrite instructions cannot first appear during a request.
func validateTransforms(r RouteConfig) error {
	if c := r.RequestTransform; c != nil {
		for _, value := range c.Headers.Add {
			if value == "$response_time" {
				return fmt.Errorf("response_time is unavailable on requests")
			}
		}
		if err := validateTransformHeaders(c.Headers); err != nil {
			return err
		}
		if c.Body != nil {
			if len(c.Body.Mapping) == 0 {
				return fmt.Errorf("request body mapping must not be empty")
			}
			for dest, src := range c.Body.Mapping {
				if src == "$response_time" {
					return fmt.Errorf("response_time is unavailable on requests")
				}
				if !validFieldPath(dest) {
					return fmt.Errorf("invalid destination path %q", dest)
				}
				for other := range c.Body.Mapping {
					if strings.HasPrefix(other, dest+".") {
						return fmt.Errorf("conflicting destination paths %q and %q", dest, other)
					}
				}
				if strings.HasPrefix(src, "$") {
					if err := validateExpression(src, false); err != nil {
						return err
					}
				} else if !validFieldPath(src) {
					return fmt.Errorf("invalid source path %q", src)
				}
			}
		}
	}
	if c := r.ResponseTransform; c != nil {
		if err := validateTransformHeaders(c.Headers); err != nil {
			return err
		}
		if c.Body != nil {
			if len(c.Body.Envelope) == 0 {
				return fmt.Errorf("response envelope must not be empty")
			}
			return validateEnvelope(c.Body.Envelope)
		}
	}
	return nil
}

// applyTransformHeaders removes configured names, then adds resolved header values.
func applyTransformHeaders(h http.Header, c TransformHeaders, v transformValues) {
	for _, k := range c.Remove {
		h.Del(k)
	}
	for k, value := range c.Add {
		h.Set(k, transformExpression(value, v, nil).(string))
	}
}

// transformJSON reads bounded identity-encoded JSON, requires one value, and preserves
// numeric text with UseNumber. Its status describes a client-side validation failure.
func transformJSON(reader io.Reader, h http.Header) (any, int, error) {
	data, err := io.ReadAll(io.LimitReader(reader, transformBodyLimit+1))
	if err != nil {
		return nil, 400, err
	}
	if len(data) > transformBodyLimit {
		return nil, 413, fmt.Errorf("body too large")
	}
	if len(data) == 0 {
		return nil, 0, nil
	}
	if enc := h.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return nil, 415, fmt.Errorf("unsupported encoding")
	}
	media, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil || media != "application/json" && !strings.HasSuffix(media, "+json") {
		return nil, 415, fmt.Errorf("unsupported media type")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var result any
	if err = d.Decode(&result); err != nil {
		return nil, 400, err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, 400, fmt.Errorf("expected one JSON value")
	}
	return result, 200, nil
}

// cleanRepresentationHeaders discards metadata that described the old bytes,
// such as length, compression, checksums, and cache validators, after JSON rewriting.
func cleanRepresentationHeaders(h http.Header) {
	for _, k := range []string{"Content-Length", "Content-Encoding", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest", "ETag", "Last-Modified", "Content-Range", "Accept-Ranges"} {
		h.Del(k)
	}
	h.Set("Content-Type", "application/json")
}

// Check expansion before marshaling: a mapping can repeat the same large value.
func transformedJSONSize(v any, remaining int) (int, error) {
	size := 0
	add := func(n int) error {
		size += n
		if size > remaining {
			return fmt.Errorf("transformed body too large")
		}
		return nil
	}
	switch x := v.(type) {
	case map[string]any:
		if err := add(2); err != nil {
			return 0, err
		}
		i := 0
		for key, value := range x {
			encoded, _ := json.Marshal(key)
			overhead := len(encoded) + 1
			if i > 0 {
				overhead++
			}
			i++
			if err := add(overhead); err != nil {
				return 0, err
			}
			n, err := transformedJSONSize(value, remaining-size)
			if err != nil {
				return 0, err
			}
			if err = add(n); err != nil {
				return 0, err
			}
		}
	case []any:
		if err := add(2); err != nil {
			return 0, err
		}
		for i, value := range x {
			if i > 0 {
				if err := add(1); err != nil {
					return 0, err
				}
			}
			n, err := transformedJSONSize(value, remaining-size)
			if err != nil {
				return 0, err
			}
			if err = add(n); err != nil {
				return 0, err
			}
		}
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return 0, err
		}
		if err = add(len(encoded)); err != nil {
			return 0, err
		}
	}
	return size, nil
}

// encodeTransformed checks output expansion before allocating the encoded body
// and clears metadata that no longer matches its bytes.
func encodeTransformed(v any, h http.Header) ([]byte, error) {
	if _, err := transformedJSONSize(v, transformBodyLimit); err != nil {
		return nil, err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) > transformBodyLimit {
		return nil, fmt.Errorf("transformed body too large")
	}
	cleanRepresentationHeaders(h)
	return b, nil
}

// mappedBody replaces the request object using destination/source dot paths.
// Missing source fields become null; expressions can supply metadata or literal values.
func mappedBody(input map[string]any, mapping map[string]string, v transformValues) map[string]any {
	out := map[string]any{}
	for dest, src := range mapping {
		var value any
		if strings.HasPrefix(src, "$") {
			value = transformExpression(src, v, nil)
		} else {
			value = input
			for _, part := range strings.Split(src, ".") {
				m, ok := value.(map[string]any)
				if !ok {
					value = nil
					break
				}
				value = m[part]
			}
		}
		parts := strings.Split(dest, ".")
		m := out
		for _, part := range parts[:len(parts)-1] {
			if m[part] == nil {
				m[part] = map[string]any{}
			}
			m = m[part].(map[string]any)
		}
		m[parts[len(parts)-1]] = value
	}
	return out
}

// envelopeBody recursively builds the configured response wrapper, substituting
// $body with the parsed final backend response and resolving metadata tokens.
func envelopeBody(value any, v transformValues, body any) any {
	switch x := value.(type) {
	case string:
		return transformExpression(x, v, body)
	case map[string]any:
		out := map[string]any{}
		for k, item := range x {
			out[k] = envelopeBody(item, v, body)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = envelopeBody(item, v, body)
		}
		return out
	default:
		return value
	}
}

// transformRequest applies request rules once before any backend attempt.
// Empty bodies stay empty; JSON mapping replaces the body and updates its length.
func (g *Gateway) transformRequest(out *http.Request, r *route, v transformValues) (int, error) {
	c := r.config.RequestTransform
	if c == nil {
		return 0, nil
	}
	transformed := false
	if c.Body != nil && out.Body != nil {
		originalBody := out.Body
		defer originalBody.Close()
		body, status, err := transformJSON(out.Body, out.Header)
		if err != nil {
			return status, err
		}
		if status == 0 {
			out.Body = http.NoBody
			out.ContentLength = 0
			out.GetBody = nil
		}
		if status != 0 {
			obj, ok := body.(map[string]any)
			if !ok {
				return 400, fmt.Errorf("mapping requires object")
			}
			b, err := encodeTransformed(mappedBody(obj, c.Body.Mapping, v), out.Header)
			if err != nil {
				return 413, err
			}
			transformed = true
			out.Body = io.NopCloser(bytes.NewReader(b))
			out.ContentLength = int64(len(b))
			out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
		}
	}
	applyTransformHeaders(out.Header, c.Headers, v)
	if transformed {
		cleanRepresentationHeaders(out.Header)
	}
	return 0, nil
}

// transformResponse rewrites only the final backend response. HEAD, 204, and 304
// retain their bodyless semantics; header-only rules leave the body streamed.
func (g *Gateway) transformResponse(response *http.Response, req *http.Request, r *route, v transformValues) error {
	c := r.config.ResponseTransform
	if c == nil {
		return nil
	}
	v.responseTime = g.now().UTC().Format(time.RFC3339Nano)
	transformed := false
	if c.Body != nil && req.Method != http.MethodHead && response.StatusCode != 204 && response.StatusCode != 304 {
		body, _, err := transformJSON(response.Body, response.Header)
		if err != nil {
			return err
		}
		b, err := encodeTransformed(envelopeBody(c.Body.Envelope, v, body), response.Header)
		if err != nil {
			return err
		}
		_ = response.Body.Close()
		transformed = true
		response.Body = io.NopCloser(bytes.NewReader(b))
		response.ContentLength = int64(len(b))
	}
	applyTransformHeaders(response.Header, c.Headers, v)
	if transformed {
		cleanRepresentationHeaders(response.Header)
	}
	return nil
}
