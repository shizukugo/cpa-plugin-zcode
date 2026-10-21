package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func jsonResult(value any) ([]byte, error) { return json.Marshal(value) }

func TestCountTokensDispatchAndPassthroughKeyRedaction(t *testing.T) {
	configureRoutingTest(t)
	cfg := currentConfig()
	cfg.APIKey = ""
	activeConfig.Store(cfg)
	original := hostCall
	t.Cleanup(func() { hostCall = original })
	for _, status := range []int{200, 401, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			hostCall = func(method string, raw []byte) ([]byte, error) {
				switch method {
				case pluginabi.MethodHostHTTPOperationOpen:
					return []byte(`{"operation_id":"count-op"}`), nil
				case pluginabi.MethodHostHTTPCancel:
					return []byte(`{}`), nil
				case pluginabi.MethodHostHTTPDo:
					var req hostHTTPRequest
					if err := json.Unmarshal(raw, &req); err != nil {
						t.Fatal(err)
					}
					var body map[string]any
					if err := json.Unmarshal(req.Body, &body); err != nil {
						t.Fatal(err)
					}
					_, hasStream := body["stream"]
					if !strings.HasSuffix(req.URL, "/v1/messages/count_tokens") || hasStream || body["model"] != "glm-5.3" || req.Headers.Get("x-api-key") != "test-passthrough-key" {
						t.Fatal("count_tokens request mapping or authentication incorrect")
					}
					payload := []byte(`{"input_tokens":7}`)
					if status != 200 {
						payload = []byte("test-passthrough-key denied")
					}
					return jsonResult(pluginapi.HTTPResponse{StatusCode: status, Body: payload})
				}
				return nil, fmt.Errorf("unexpected callback %s", method)
			}
			raw, _ := json.Marshal(rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "zcode/glm-5.3", Headers: http.Header{"Authorization": {"Bearer test-passthrough-key"}}, Payload: []byte(`{"stream":true,"messages":[]}`)}})
			response, err := handleMethod(pluginabi.MethodExecutorCountTokens, raw)
			if status == 200 {
				if err != nil {
					t.Fatal(err)
				}
				var env envelope
				if err := json.Unmarshal(response, &env); err != nil {
					t.Fatal(err)
				}
				var result pluginapi.ExecutorResponse
				if err := json.Unmarshal(env.Result, &result); err != nil {
					t.Fatal(err)
				}
				if !env.OK || string(result.Payload) != `{"input_tokens":7}` {
					t.Fatal("token count response changed")
				}
			} else {
				pe, ok := err.(*pluginabi.Error)
				if !ok || pe.HTTPStatus != status || strings.Contains(err.Error(), "test-passthrough-key") {
					t.Fatalf("unsafe or incorrect error: %v", err)
				}
			}
		})
	}
}

func TestRedactionBeforeTruncation(t *testing.T) {
	key := "test-secret-key"
	err := upstreamError(429, []byte(strings.Repeat("x", 1020)+key), key)
	if strings.Contains(err.Error(), "test") {
		t.Fatal("partial key leaked at truncation boundary")
	}
	err = redactExecutorError(pluginabi.NewError("host_error", key, 502), key)
	if strings.Contains(err.Error(), key) || err.(*pluginabi.Error).HTTPStatus != 502 {
		t.Fatal("host error redaction lost status")
	}
}

func TestStreamFramesRespectCPATranslatorContract(t *testing.T) {
	frame := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n")
	frames := streamFrames(frame)
	if !bytes.Equal(bytes.Join(frames, nil), frame) {
		t.Fatal("native SSE event changed")
	}
	if len(frames) != 3 || string(frames[1]) != "data: {\"type\":\"content_block_delta\"}\n" {
		t.Fatalf("translator cannot see standalone data line: %q", frames)
	}
}

func TestExecutorDispatchMapsBothModelsAndProtectsClientKey(t *testing.T) {
	configureRoutingTest(t)
	original := hostCall
	t.Cleanup(func() { hostCall = original })
	for _, model := range []string{"zcode/glm-5.3", "zcode/glm-5.3-flash"} {
		t.Run(model, func(t *testing.T) {
			calls := 0
			hostCall = func(method string, raw []byte) ([]byte, error) {
				switch method {
				case pluginabi.MethodHostHTTPOperationOpen:
					return []byte(`{"operation_id":"test-op"}`), nil
				case pluginabi.MethodHostHTTPCancel:
					return []byte(`{}`), nil
				case pluginabi.MethodHostHTTPDo:
					calls++
					var req hostHTTPRequest
					if err := json.Unmarshal(raw, &req); err != nil {
						t.Fatal(err)
					}
					if req.URL != "https://open.bigmodel.cn/api/anthropic/v1/messages" {
						t.Fatalf("url=%s", req.URL)
					}
					if req.Headers.Get("x-api-key") != "test-upstream-key" {
						t.Fatal("fixed upstream credential missing")
					}
					if req.Headers.Get("Authorization") != "" || req.Headers.Get("Cookie") != "" {
						t.Fatal("client credentials leaked")
					}
					if req.Headers.Get("x-zcode-agent") != "glm" {
						t.Fatal("ZCode fingerprint missing")
					}
					if req.HostCallbackID != "test-callback" || req.OperationID != "test-op" {
						t.Fatal("host context missing")
					}
					var body map[string]any
					if err := json.Unmarshal(req.Body, &body); err != nil {
						t.Fatal(err)
					}
					if body["model"] != strings.TrimPrefix(model, "zcode/") || body["stream"] != false {
						t.Fatalf("bad mapped payload %#v", body)
					}
					return jsonResult(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"content":[{"type":"text","text":"OK"}]}`), Headers: http.Header{"Content-Type": {"application/json"}}})
				}
				return nil, fmt.Errorf("unexpected host method: %s", method)
			}
			req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: model, Headers: http.Header{"Authorization": {"Bearer client-secret"}, "Cookie": {"login-secret"}}, Payload: []byte(`{"model":"client-alias","messages":[]}`)}, HostCallbackID: "test-callback"}
			raw, _ := json.Marshal(req)
			response, err := handleMethod(pluginabi.MethodExecutorExecute, raw)
			if err != nil {
				t.Fatal(err)
			}
			var env envelope
			if err := json.Unmarshal(response, &env); err != nil {
				t.Fatal(err)
			}
			if !env.OK || calls != 1 {
				t.Fatalf("execute failed: calls=%d env=%#v", calls, env)
			}
		})
	}
}

func TestExecutorPropagatesUpstreamStatusAndRedactsKey(t *testing.T) {
	configureRoutingTest(t)
	original := hostCall
	t.Cleanup(func() { hostCall = original })
	hostCall = func(method string, raw []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPOperationOpen {
			return []byte(`{"operation_id":"op"}`), nil
		}
		if method == pluginabi.MethodHostHTTPCancel {
			return []byte(`{}`), nil
		}
		return jsonResult(pluginapi.HTTPResponse{StatusCode: 429, Body: []byte(`quota test-upstream-key exhausted`)})
	}
	raw, _ := json.Marshal(rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "zcode/glm-5.3", Payload: []byte(`{}`)}})
	_, err := handleMethod(pluginabi.MethodExecutorExecute, raw)
	if err == nil || strings.Contains(err.Error(), "test-upstream-key") {
		t.Fatalf("unsafe/missing error: %v", err)
	}
	pluginError, ok := err.(*pluginabi.Error)
	if !ok || pluginError.HTTPStatus != 429 {
		t.Fatalf("lost HTTP status: %#v", err)
	}
}

func TestExecutorStreamUsesHostBridgeAndClosesResources(t *testing.T) {
	for _, tc := range []struct {
		failEmit bool
		newline  string
	}{{false, "\n"}, {true, "\n"}, {false, "\r\n"}} {
		failEmit, newline := tc.failEmit, tc.newline
		t.Run(fmt.Sprintf("cancel=%v/newline=%q", failEmit, newline), func(t *testing.T) {
			configureRoutingTest(t)
			original := hostCall
			t.Cleanup(func() { hostCall = original })
			var mu sync.Mutex
			var emitted []byte
			closed := make(chan struct{})
			next := 0
			httpClosed := false
			opCancelled := false
			hostCall = func(method string, raw []byte) ([]byte, error) {
				mu.Lock()
				defer mu.Unlock()
				switch method {
				case pluginabi.MethodHostHTTPOperationOpen:
					return []byte(`{"operation_id":"stream-op"}`), nil
				case pluginabi.MethodHostHTTPCancel:
					opCancelled = true
				case pluginabi.MethodHostHTTPDoStream:
					var req hostHTTPRequest
					if err := json.Unmarshal(raw, &req); err != nil {
						return nil, err
					}
					var body map[string]any
					if err := json.Unmarshal(req.Body, &body); err != nil {
						return nil, err
					}
					if body["stream"] != true {
						return nil, fmt.Errorf("stream flag missing")
					}
					return jsonResult(hostStreamResponse{StatusCode: 200, StreamID: "upstream", Headers: http.Header{"Content-Type": {"text/event-stream"}}})
				case pluginabi.MethodHostHTTPStreamRead:
					next++
					if next == 1 {
						return jsonResult(hostStreamRead{Payload: []byte("event: message_stop" + newline + "da")})
					}
					if next == 2 {
						return jsonResult(hostStreamRead{Payload: []byte("ta: {}" + newline + newline), Done: true})
					}
					return nil, fmt.Errorf("read after EOF")
				case pluginabi.MethodHostStreamEmit:
					if failEmit {
						return nil, fmt.Errorf("downstream closed")
					}
					var req rpcStreamEmitRequest
					if err := json.Unmarshal(raw, &req); err != nil {
						return nil, err
					}
					emitted = append(emitted, req.Payload...)
				case pluginabi.MethodHostHTTPStreamClose:
					httpClosed = true
				case pluginabi.MethodHostStreamClose:
					close(closed)
				default:
					return nil, fmt.Errorf("unexpected host method: %s", method)
				}
				return []byte(`{}`), nil
			}
			raw, _ := json.Marshal(rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "zcode/glm-5.3-flash", Payload: []byte(`{}`)}, StreamID: "downstream", HostCallbackID: "context"})
			if _, err := handleMethod(pluginabi.MethodExecutorExecuteStream, raw); err != nil {
				t.Fatal(err)
			}
			select {
			case <-closed:
			case <-time.After(3 * time.Second):
				t.Fatal("stream never closed")
			}
			mu.Lock()
			defer mu.Unlock()
			if !httpClosed || !opCancelled {
				t.Fatal("upstream stream/operation leaked")
			}
			if !failEmit && string(emitted) != "event: message_stop"+newline+"data: {}"+newline+newline {
				t.Fatalf("corrupt stream %q", emitted)
			}
		})
	}
}
