package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// RawCaller performs one host callback round-trip: method name plus JSON
// request bytes in, JSON envelope bytes out.
type RawCaller func(method string, payload []byte) ([]byte, error)

// HostClient is the subset of host callbacks this plugin uses. Tests inject a
// fake; main.go wires the C host API.
type HostClient interface {
	AuthList(ctx context.Context) ([]pluginapi.HostAuthFileEntry, error)
	AuthGet(ctx context.Context, authIndex string) (pluginapi.HostAuthGetResponse, error)
	AuthSave(ctx context.Context, req pluginapi.HostAuthSaveRequest) error
	Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
	Log(level, message string, fields map[string]any)
}

// HostBridge routes calls through the host callbacks "host.auth.*",
// "host.http.do" and "host.log". Never place keys, tokens or response bodies
// in Log messages or error text.
type HostBridge struct {
	call     RawCaller
	inFlight sync.WaitGroup
}

func NewHostBridge(call RawCaller) *HostBridge { return &HostBridge{call: call} }

type hostHTTPReq struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Headers http.Header `json:"headers"`
	Body    []byte      `json:"body"`
}

type hostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type hostLogReq struct {
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields"`
}

// callWithTimeout runs the raw host call on its own goroutine so a stalled
// host never blocks the caller past ctx. A late result is dropped. Panics
// inside the callback degrade to an error rather than killing the host.
func (b *HostBridge) callWithTimeout(ctx context.Context, method string, payload []byte) ([]byte, error) {
	if b == nil || b.call == nil {
		return nil, fmt.Errorf("host bridge unavailable")
	}
	type result struct {
		raw []byte
		err error
	}
	ch := make(chan result, 1)
	b.inFlight.Add(1)
	go func() {
		defer b.inFlight.Done()
		defer func() {
			if r := recover(); r != nil {
				ch <- result{err: fmt.Errorf("host callback %s panicked", method)}
			}
		}()
		raw, err := b.call(method, payload)
		ch <- result{raw: raw, err: err}
	}()
	select {
	case r := <-ch:
		return r.raw, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("host callback %s timed out", method)
	}
}

func (b *HostBridge) invoke(ctx context.Context, method string, payload any, what string) (pluginabi.Envelope, error) {
	payloadBytes, _ := json.Marshal(payload)
	raw, err := b.callWithTimeout(ctx, method, payloadBytes)
	if err != nil {
		return pluginabi.Envelope{}, fmt.Errorf("%s failed: %w", what, err)
	}
	env, err := decodeEnvelope(raw)
	if err != nil {
		return pluginabi.Envelope{}, fmt.Errorf("%s failed: %w", what, err)
	}
	if !env.OK {
		msg := ""
		if env.Error != nil {
			msg = env.Error.Message
		}
		return pluginabi.Envelope{}, fmt.Errorf("%s failed: %s", what, msg)
	}
	return env, nil
}

func (b *HostBridge) AuthList(ctx context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	env, err := b.invoke(ctx, pluginabi.MethodHostAuthList, struct{}{}, "host auth list")
	if err != nil {
		return nil, err
	}
	var resp hostAuthListResponse
	if len(env.Result) > 0 && json.Unmarshal(env.Result, &resp) != nil {
		return nil, fmt.Errorf("host auth list failed: undecodable response body")
	}
	return resp.Files, nil
}

func (b *HostBridge) AuthGet(ctx context.Context, authIndex string) (pluginapi.HostAuthGetResponse, error) {
	env, err := b.invoke(ctx, pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex}, "host auth get")
	if err != nil {
		return pluginapi.HostAuthGetResponse{}, err
	}
	var resp pluginapi.HostAuthGetResponse
	if len(env.Result) > 0 && json.Unmarshal(env.Result, &resp) != nil {
		return pluginapi.HostAuthGetResponse{}, fmt.Errorf("host auth get failed: undecodable response body")
	}
	return resp, nil
}

// AuthSave persists one plugin-owned auth record through the host. Host error
// text is not relayed because it may echo credential JSON.
func (b *HostBridge) AuthSave(ctx context.Context, req pluginapi.HostAuthSaveRequest) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("host auth save failed: invalid request")
	}
	raw, err := b.callWithTimeout(ctx, pluginabi.MethodHostAuthSave, payload)
	if err != nil {
		return fmt.Errorf("host auth save failed")
	}
	env, err := decodeEnvelope(raw)
	if err != nil || !env.OK {
		return fmt.Errorf("host auth save failed")
	}
	return nil
}

// Do issues an upstream HTTP request through "host.http.do" so the host's
// proxy and transport policy apply.
func (b *HostBridge) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	env, err := b.invoke(ctx, pluginabi.MethodHostHTTPDo, hostHTTPReq{Method: req.Method, URL: req.URL, Headers: req.Headers, Body: req.Body}, "host http do")
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	var resp pluginapi.HTTPResponse
	if len(env.Result) > 0 && json.Unmarshal(env.Result, &resp) != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("host http do failed: undecodable response body")
	}
	return resp, nil
}

// Log forwards one secret-free diagnostic record to "host.log" (best effort).
func (b *HostBridge) Log(level, message string, fields map[string]any) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = b.invoke(ctx, pluginabi.MethodHostLog, hostLogReq{Level: level, Message: message, Fields: fields}, "host log")
}

// Wait blocks until outstanding host callbacks finish or timeout elapses.
// Unix loaders dlclose the plugin right after shutdown returns, so no
// goroutine may still be calling into the host afterwards.
func (b *HostBridge) Wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() { b.inFlight.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
