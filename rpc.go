package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
)

// The JSON-RPC 2.0 messages MCP speaks, and the request/response matching
// shared by the two upstreams that carry a stream of messages (stdio and the
// older HTTP+SSE transport). Streamable HTTP answers each POST on its own
// connection and uses only the types.

// protocolVersion is the MCP version the proxy asks for. An upstream may
// answer with an older one; the proxy then speaks that one.
const protocolVersion = "2025-06-18"

// maxMessageBytes caps one message from the upstream: the gateway's own cap
// on a request body, applied to the answer too.
const maxMessageBytes = 10 << 20

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (m message) isResponse() bool { return len(m.ID) > 0 && m.Method == "" }
func (m message) isRequest() bool  { return len(m.ID) > 0 && m.Method != "" }

// rpcError is an error the upstream ANSWERED with, as opposed to a failure
// to reach it.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("upstream error %d: %s", e.Code, e.Message) }

// The JSON-RPC codes the proxy maps to an HTTP status (proxy.go).
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// errUnreachable wraps every failure to get an answer from the upstream.
var errUnreachable = errors.New("upstream unreachable")

func unreachable(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errUnreachable, fmt.Sprintf(format, args...))
}

// encodeRequest writes a request by hand rather than through json.Marshal,
// which re-compacts and HTML-escapes a json.RawMessage: params given as raw
// bytes (a tool call's arguments) reach the upstream exactly as sent.
func encodeRequest(id int64, method string, params any) ([]byte, error) {
	raw, ok := params.(json.RawMessage)
	if !ok {
		encoded, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("encode %s: %w", method, err)
		}
		raw = encoded
	}
	name, err := json.Marshal(method)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", method, err)
	}
	var b bytes.Buffer
	b.WriteString(`{"jsonrpc":"2.0","id":`)
	b.WriteString(strconv.FormatInt(id, 10))
	b.WriteString(`,"method":`)
	b.Write(name)
	b.WriteString(`,"params":`)
	b.Write(raw)
	b.WriteByte('}')
	return b.Bytes(), nil
}

func encodeNotification(method string, params any) ([]byte, error) {
	m := message{JSONRPC: "2.0", Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("encode %s: %w", method, err)
		}
		m.Params = raw
	}
	return json.Marshal(m)
}

// answerServerRequest is what the proxy says to a request FROM the upstream.
// It declares no client capabilities, so only `ping` is owed an answer; the
// rest are refused as unknown methods rather than left hanging.
func answerServerRequest(request message) []byte {
	reply := message{JSONRPC: "2.0", ID: request.ID}
	if request.Method == "ping" {
		reply.Result = json.RawMessage(`{}`)
	} else {
		reply.Error = &rpcError{Code: codeMethodNotFound, Message: "the Nexus402 verify-proxy does not offer " + request.Method}
	}
	raw, _ := json.Marshal(reply) // fields are all marshalable
	return raw
}

func initializeParams() map[string]any {
	return map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "nexus402-verify-proxy", "version": version},
	}
}

// negotiated reads the version an upstream's initialize result agreed to.
func negotiated(result json.RawMessage) string {
	var r struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(result, &r) != nil || r.ProtocolVersion == "" {
		return protocolVersion
	}
	return r.ProtocolVersion
}

// streamSession matches responses to requests over one stream of messages.
// send writes one message; the owner's reader calls deliver for each message
// read and end when the stream stops.
type streamSession struct {
	send     func([]byte) error
	onNotify func(method string)

	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[string]chan message
	done    chan struct{}
	err     error
}

func newStreamSession(send func([]byte) error, onNotify func(string)) *streamSession {
	return &streamSession{send: send, onNotify: onNotify, pending: map[string]chan message{}, done: make(chan struct{})}
}

// request sends one request and waits for its answer, the context, or the
// end of the stream. A cancelled request is cancelled upstream too.
func (s *streamSession) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := s.nextID.Add(1)
	raw, err := encodeRequest(id, method, params)
	if err != nil {
		return nil, err
	}
	key := strconv.FormatInt(id, 10)
	reply := make(chan message, 1)
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return nil, s.err
	}
	s.pending[key] = reply
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
	}()
	if err := s.send(raw); err != nil {
		return nil, unreachable("send %s: %v", method, err)
	}
	select {
	case m := <-reply:
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	case <-s.done:
		return nil, s.ended()
	case <-ctx.Done():
		if cancel, err := encodeNotification("notifications/cancelled", map[string]any{"requestId": id, "reason": "the caller went away"}); err == nil {
			_ = s.send(cancel)
		}
		return nil, unreachable("%s: %v", method, ctx.Err())
	}
}

func (s *streamSession) notify(method string, params any) error {
	raw, err := encodeNotification(method, params)
	if err != nil {
		return err
	}
	return s.send(raw)
}

// deliver routes one message read from the stream.
func (s *streamSession) deliver(m message) {
	switch {
	case m.isResponse():
		s.mu.Lock()
		reply, ok := s.pending[string(m.ID)]
		s.mu.Unlock()
		if ok {
			reply <- m
		}
	case m.isRequest():
		_ = s.send(answerServerRequest(m))
	case m.Method != "" && s.onNotify != nil:
		s.onNotify(m.Method)
	}
}

// end fails every waiting request and every later one with err.
func (s *streamSession) end(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return
	}
	s.err = unreachable("%v", err)
	close(s.done)
}

func (s *streamSession) ended() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *streamSession) alive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}
