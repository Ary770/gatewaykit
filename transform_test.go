package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProvidedTransformsEndToEnd(t *testing.T) {
	c, _, err := loadConfig("gateway.yaml")
	if err != nil {
		t.Fatal(err)
	}
	route := c.Routes[3]
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body map[string]any
		d := json.NewDecoder(strings.NewReader(string(data)))
		d.UseNumber()
		if err := d.Decode(&body); err != nil {
			t.Error(err)
		}
		user := body["user"].(map[string]any)
		meta := body["meta"].(map[string]any)
		if user["id"] != json.Number("9007199254740993") || user["name"] != "Ada" || meta["source"] != "gateway" || meta["timestamp"] != "2026-01-01T00:00:00Z" {
			t.Errorf("mapping %s", data)
		}
		if r.Header.Get("X-Gateway") != "gatewaykit" || r.Header.Get("X-Debug") != "" || r.Header.Get("ETag") != "" || r.Header.Get("X-Request-Start") != "2026-01-01T00:00:00Z" {
			t.Errorf("headers %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Server", "secret")
		w.Header().Set("ETag", "stale")
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	route.Upstream.URL = upstream.URL
	g := testGateway(t, route)
	g.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	req := httptest.NewRequest("POST", "/api/legacy", strings.NewReader(`{"userId":9007199254740993,"userName":"Ada","ignored":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Debug", "yes")
	req.Header.Set("ETag", "stale")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	want := `{"data":{"ok":true},"gateway_metadata":{"route":"/api/legacy","served_at":"2026-01-01T00:00:00Z"}}`
	if w.Code != 201 || w.Body.String() != want || w.Header().Get("Server") != "" || w.Header().Get("ETag") != "" || w.Header().Get("X-Served-By") != "gatewaykit" {
		t.Fatalf("%d %s %v", w.Code, w.Body.String(), w.Header())
	}
}

func TestRequestTransformFailuresAndEmptyBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { b, _ := io.ReadAll(r.Body); _, _ = w.Write(b) }))
	defer upstream.Close()
	route := RouteConfig{Path: "/", Methods: []string{"POST"}, Upstream: UpstreamConfig{URL: upstream.URL}, RequestTransform: &RequestTransformConfig{Body: &RequestTransformBody{Mapping: map[string]string{"out": "input"}}}}
	for _, tc := range []struct {
		name, body, media, encoding string
		status                      int
		want                        string
	}{
		{"missing", `{}`, "application/json", "", 200, `{"out":null}`}, {"null", `{"input":null}`, "application/json", "", 200, `{"out":null}`}, {"array value", `{"input":[1,2]}`, "application/problem+json", "", 200, `{"out":[1,2]}`}, {"empty", "", "", "", 200, ""},
		{"invalid", `{`, "application/json", "", 400, ""}, {"multiple", `{} {}`, "application/json", "", 400, ""}, {"root array", `[]`, "application/json", "", 400, ""}, {"root null", `null`, "application/json", "", 400, ""}, {"media", `{}`, "text/plain", "", 415, ""}, {"encoding", `{}`, "application/json", "gzip", 415, ""}, {"oversize", strings.Repeat("x", transformBodyLimit+1), "application/json", "", 413, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := testGateway(t, route)
			req := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.media)
			req.Header.Set("Content-Encoding", tc.encoding)
			w := httptest.NewRecorder()
			g.ServeHTTP(w, req)
			if w.Code != tc.status || tc.status == 200 && w.Body.String() != tc.want {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
}
func TestResponseTransformFailuresAndBodylessStatuses(t *testing.T) {
	for _, tc := range []struct {
		name, body, media, encoding, method string
		upstreamStatus, want                int
	}{
		{"invalid", "{", "application/json", "", "GET", 200, 502}, {"media", "hi", "text/plain", "", "GET", 200, 502}, {"encoding", "{}", "application/json", "gzip", "GET", 200, 502}, {"large", strings.Repeat("x", transformBodyLimit+1), "application/json", "", "GET", 200, 502}, {"head", "", "", "", "HEAD", 200, 200}, {"no content", "", "", "", "GET", 204, 204}, {"not modified", "", "", "", "GET", 304, 304},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.media)
				w.Header().Set("Content-Encoding", tc.encoding)
				w.WriteHeader(tc.upstreamStatus)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			g := testGateway(t, RouteConfig{Path: "/", Methods: []string{tc.method}, Upstream: UpstreamConfig{URL: upstream.URL}, ResponseTransform: &ResponseTransformConfig{Body: &ResponseTransformBody{Envelope: map[string]any{"data": "$body"}}}})
			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(tc.method, "/", nil))
			if w.Code != tc.want {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
}
func TestTransformValidation(t *testing.T) {
	for _, fragment := range []string{
		`request_transform: {unknown: true}`, `request_transform: {body: {mapping: {a: x, a.b: y}}}`, `request_transform: {body: {mapping: {a: "$unknown"}}}`, `request_transform: {body: {mapping: {a: "x..y"}}}`, `request_transform: {body: {mapping: {"a[]": x}}}`, `request_transform: {body: {mapping: {}}}`, `response_transform: {body: {envelope: {}}}`, `response_transform: {body: {envelope: {data: "$unknown"}}}`, `request_transform: {headers: {add: {Connection: close}}}`, `request_transform: {headers: {remove: [X-Forwarded-For]}}`, `request_transform: {headers: {add: {X-Test: "$body"}}}`, `request_transform: {headers: {add: {"Bad Header": x}}}`, `request_transform: {headers: {add: {X-Test: "a\nb"}}}`, `response_transform: {headers: {add: {X-Test: x, x-test: y}}}`,
	} {
		t.Run(fragment, func(t *testing.T) {
			if _, _, err := decodeConfig(strings.NewReader(minimalConfig + "    " + fragment + "\n")); err == nil {
				t.Fatal("accepted malformed transform")
			}
		})
	}
}
func TestTransformedOutputLimit(t *testing.T) {
	h := http.Header{}
	if _, err := encodeTransformed(strings.Repeat("x", transformBodyLimit), h); err == nil {
		t.Fatal("accepted oversized output")
	}
}
func TestEnvelopeNestedValues(t *testing.T) {
	v := envelopeBody(map[string]any{"nested": []any{"$body", "$literal:hello", true, nil}}, transformValues{}, json.Number("123"))
	b, _ := json.Marshal(v)
	if string(b) != `{"nested":[123,"hello",true,null]}` {
		t.Fatal(string(b))
	}
}
func TestTransformReadDeadline(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: upstream.URL, Timeout: "20ms"}, ResponseTransform: &ResponseTransformConfig{Body: &ResponseTransformBody{Envelope: map[string]any{"data": "$body"}}}})
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 504 {
		t.Fatalf("got %d", w.Code)
	}
}

func TestHeaderOnlyTransformsStreamLargeBodies(t *testing.T) {
	payload := strings.Repeat("x", transformBodyLimit+1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Added") != "yes" {
			t.Error("missing header")
		}
		_, _ = io.Copy(w, r.Body)
	}))
	defer upstream.Close()
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"POST"}, Upstream: UpstreamConfig{URL: upstream.URL}, RequestTransform: &RequestTransformConfig{Headers: TransformHeaders{Add: map[string]string{"X-Added": "yes"}}}, ResponseTransform: &ResponseTransformConfig{Headers: TransformHeaders{Add: map[string]string{"X-Reply": "yes"}}}})
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("POST", "/", strings.NewReader(payload)))
	if w.Code != 200 || w.Body.String() != payload || w.Header().Get("X-Reply") != "yes" {
		t.Fatalf("code=%d bytes=%d", w.Code, w.Body.Len())
	}
}
func TestTransformOutputExpansionRejected(t *testing.T) {
	data := `{"input":"` + strings.Repeat("x", transformBodyLimit/2) + `"}`
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"POST"}, Upstream: UpstreamConfig{URL: "http://127.0.0.1:1"}, RequestTransform: &RequestTransformConfig{Body: &RequestTransformBody{Mapping: map[string]string{"one": "input", "two": "input"}}}})
	req := httptest.NewRequest("POST", "/", strings.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != 413 {
		t.Fatalf("got %d", w.Code)
	}
}
func TestRequestTransformUploadDeadline(t *testing.T) {
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"POST"}, Upstream: UpstreamConfig{URL: "http://127.0.0.1:1", Timeout: "30ms"}, RequestTransform: &RequestTransformConfig{Body: &RequestTransformBody{Mapping: map[string]string{"out": "in"}}}})
	server := httptest.NewServer(g)
	defer server.Close()
	reader, writer := io.Pipe()
	defer writer.Close()
	req, _ := http.NewRequest("POST", server.URL, reader)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: time.Second}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 504 {
		t.Fatalf("got %d", response.StatusCode)
	}
}

func TestResponseEnvelopeExpansionRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `"`+strings.Repeat("x", transformBodyLimit/2)+`"`)
	}))
	defer upstream.Close()
	g := testGateway(t, RouteConfig{Path: "/", Methods: []string{"GET"}, Upstream: UpstreamConfig{URL: upstream.URL}, ResponseTransform: &ResponseTransformConfig{Body: &ResponseTransformBody{Envelope: map[string]any{"one": "$body", "two": "$body"}}}})
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 502 {
		t.Fatalf("got %d", w.Code)
	}
}
func TestTransformedJSONSizeMatchesEncoding(t *testing.T) {
	for _, value := range []any{nil, true, 123, `<>&"`, []any{1, "a", map[string]any{"x": nil}}, map[string]any{"a": []any{true, false}, "b": "x"}} {
		b, _ := json.Marshal(value)
		n, err := transformedJSONSize(value, len(b))
		if err != nil || n != len(b) {
			t.Fatalf("%s size=%d err=%v", b, n, err)
		}
		if _, err := transformedJSONSize(value, len(b)-1); err == nil {
			t.Fatalf("accepted over budget %s", b)
		}
	}
}

type closeObservedBody struct {
	io.Reader
	closed bool
}

func (b *closeObservedBody) Close() error { b.closed = true; return nil }
func TestRequestTransformClosesOriginalBody(t *testing.T) {
	for _, payload := range []string{`{"x":1}`, "", "{"} {
		body := &closeObservedBody{Reader: strings.NewReader(payload)}
		req := httptest.NewRequest("POST", "/", nil)
		req.Body = body
		req.Header.Set("Content-Type", "application/json")
		g := &Gateway{}
		r := &route{config: RouteConfig{RequestTransform: &RequestTransformConfig{Body: &RequestTransformBody{Mapping: map[string]string{"out": "x"}}}}}
		_, _ = g.transformRequest(req, r, transformValues{})
		if !body.closed {
			t.Fatalf("body not closed for %q", payload)
		}
	}
}
