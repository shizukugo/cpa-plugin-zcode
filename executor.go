package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rensumo/cpa-plugin-zcode/internal/mimic"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Replaced by in-process fakes in regression tests; production uses the C ABI.
var hostCall = callHostABI

type hostHTTPRequest struct {
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	OperationID    string      `json:"operation_id,omitempty"`
	Method         string      `json:"method"`
	URL            string      `json:"url"`
	Headers        http.Header `json:"headers"`
	Body           []byte      `json:"body"`
}
type hostStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
}
type hostStreamRead struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

func invokeHost(method string, request any, response any) error {
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	result, err := hostCall(method, raw)
	if err != nil {
		return err
	}
	if response == nil {
		return nil
	}
	return json.Unmarshal(result, response)
}

func prepareUpstream(req rpcExecutorRequest, path string, stream bool) (hostHTTPRequest, error) {
	cfg := currentConfig()
	known := false
	for _, model := range cfg.Models {
		if model == req.Model {
			known = true
			break
		}
	}
	if !known && !dynamicModelsEnabled(cfg) {
		return hostHTTPRequest{}, pluginabi.NewError("unsupported_model", "Model not configured for ZCode", http.StatusBadRequest)
	}
	apiKey, err := requestAPIKey(req.ExecutorRequest, cfg)
	if err != nil {
		return hostHTTPRequest{}, err
	}
	var body map[string]any
	if err := json.Unmarshal(req.Payload, &body); err != nil || body == nil {
		return hostHTTPRequest{}, pluginabi.NewError("invalid_payload", "Expected a JSON object", http.StatusBadRequest)
	}
	body["model"] = mappedModel(req.Model, cfg)
	if path == "/v1/messages" {
		body["stream"] = stream
	} else {
		delete(body, "stream")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return hostHTTPRequest{}, err
	}
	// Do not forward CPA's client key, cookies or management headers upstream.
	headers := make(http.Header)
	mimic.New(cfg.Mimic, keyID(apiKey)).ApplyRequest(headers, apiKey, mimic.UUID4())
	headers.Set("Content-Type", "application/json")
	headers.Set("anthropic-version", "2023-06-01")
	return hostHTTPRequest{HostCallbackID: req.HostCallbackID, Method: http.MethodPost,
		URL: cfg.BaseURL + path, Headers: headers, Body: payload}, nil
}

// An operation timer cancels stalled upstream work without breaking host logging.
func openOperation(req *hostHTTPRequest) (func(), error) {
	var op struct {
		OperationID string `json:"operation_id"`
	}
	err := invokeHost(pluginabi.MethodHostHTTPOperationOpen,
		map[string]string{"host_callback_id": req.HostCallbackID}, &op)
	if err != nil {
		return nil, err
	}
	req.OperationID = op.OperationID
	cancel := func() {
		_ = invokeHost(pluginabi.MethodHostHTTPCancel,
			map[string]string{"host_callback_id": req.HostCallbackID, "operation_id": op.OperationID}, nil)
	}
	timer := time.AfterFunc(time.Duration(currentConfig().TimeoutSeconds)*time.Second, cancel)
	return func() { timer.Stop(); cancel() }, nil
}

func upstreamError(status int, body []byte, key string) error {
	message := string(body)
	if key != "" {
		message = strings.ReplaceAll(message, key, "[REDACTED]")
	}
	message = truncate(message, 1024)
	return pluginabi.NewError("upstream_error", fmt.Sprintf("ZCode upstream HTTP %d: %s", status, message), status)
}

func redactExecutorError(err error, key string) error {
	if err == nil || key == "" {
		return err
	}
	message := strings.ReplaceAll(err.Error(), key, "[REDACTED]")
	var pluginErr *pluginabi.Error
	if errors.As(err, &pluginErr) {
		redacted := *pluginErr
		redacted.Message = message
		return &redacted
	}
	return errors.New(message)
}

func executeOnce(req rpcExecutorRequest, path string) (result []byte, resultErr error) {
	upstream, err := prepareUpstream(req, path, false)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = redactExecutorError(resultErr, upstream.Headers.Get("x-api-key")) }()
	cleanup, err := openOperation(&upstream)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	var response pluginapi.HTTPResponse
	if err := invokeHost(pluginabi.MethodHostHTTPDo, upstream, &response); err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, upstreamError(response.StatusCode, response.Body, upstream.Headers.Get("x-api-key"))
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: response.Body, Headers: response.Headers})
}

func closeHTTPStream(id string) {
	_ = invokeHost(pluginabi.MethodHostHTTPStreamClose, map[string]string{"stream_id": id}, nil)
}

func executeStream(req rpcExecutorRequest) (result []byte, resultErr error) {
	if req.StreamID == "" {
		return nil, fmt.Errorf("stream bridge id missing")
	}
	upstream, err := prepareUpstream(req, "/v1/messages", true)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = redactExecutorError(resultErr, upstream.Headers.Get("x-api-key")) }()
	cleanup, err := openOperation(&upstream)
	if err != nil {
		return nil, err
	}
	var response hostStreamResponse
	if err := invokeHost(pluginabi.MethodHostHTTPDoStream, upstream, &response); err != nil {
		cleanup()
		return nil, err
	}
	if response.StreamID == "" {
		cleanup()
		return nil, fmt.Errorf("upstream stream id missing")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer cleanup()
		defer closeHTTPStream(response.StreamID)
		var body []byte
		for len(body) < 4096 {
			var chunk hostStreamRead
			if err := invokeHost(pluginabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": response.StreamID}, &chunk); err != nil {
				break
			}
			body = append(body, chunk.Payload...)
			if chunk.Done || chunk.Error != "" {
				break
			}
		}
		return nil, upstreamError(response.StatusCode, body, upstream.Headers.Get("x-api-key"))
	}
	go pumpStream(req.StreamID, response.StreamID, upstream.Headers.Get("x-api-key"), cleanup)
	return okEnvelope(struct{ Headers http.Header }{Headers: response.Headers})
}

// CPA overwrites SourceFormat with the plugin's input format, so it cannot
// distinguish native clients here. Emit individual SSE lines: their concatenation
// preserves native events exactly and translators can consume data: lines.
func streamFrames(frame []byte) [][]byte {
	var frames [][]byte
	for _, line := range bytes.SplitAfter(frame, []byte("\n")) {
		if len(line) > 0 {
			frames = append(frames, line)
		}
	}
	return frames
}

func pumpStream(outputID, inputID, apiKey string, cleanup func()) {
	var streamErr string
	defer func() {
		if recovered := recover(); recovered != nil {
			streamErr = "ZCode stream pump failed"
		}
		closeHTTPStream(inputID)
		cleanup()
		if apiKey != "" {
			streamErr = strings.ReplaceAll(streamErr, apiKey, "[REDACTED]")
		}
		_ = invokeHost(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{StreamID: outputID, Error: streamErr}, nil)
	}()
	var pending []byte
	emit := func(frame []byte) bool {
		for _, chunk := range streamFrames(frame) {
			if err := invokeHost(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{StreamID: outputID, Payload: chunk}, nil); err != nil {
				streamErr = err.Error()
				return false
			}
		}
		return true
	}
	for {
		var chunk hostStreamRead
		if err := invokeHost(pluginabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": inputID}, &chunk); err != nil {
			streamErr = err.Error()
			return
		}
		pending = append(pending, chunk.Payload...)
		for {
			end := bytes.IndexByte(pending, '\n')
			if end < 0 {
				break
			}
			if end+1 > 1<<20 {
				streamErr = "SSE line exceeds limit"
				return
			}
			if !emit(pending[:end+1]) {
				return
			}
			pending = pending[end+1:]
		}
		if len(pending) > 1<<20 {
			streamErr = "SSE line exceeds limit"
			return
		}
		if chunk.Error != "" {
			streamErr = chunk.Error
			return
		}
		if chunk.Done {
			if len(pending) > 0 {
				emit(pending)
			}
			return
		}
	}
}
