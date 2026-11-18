package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The gateway's own cap on a request body. Anything larger did not come from it.
const maxBodyBytes = 10 << 20

// callTimeout matches the gateway's hard timeout on a creator call.
const callTimeout = 120 * time.Second

// proxy is the public endpoint: it verifies every request exactly as the
// starter kits do, answers the hello and the host challenge itself, and
// forwards each verified tools/call to the upstream with only the tool's
// name changed.
type proxy struct {
	verifier  Verifier
	up        upstream
	tools     *toolNames
	challenge challenge
}

// challenge is ADR-0200's host proof, served at the root of this host when
// configured: GET of path answers 200 with content.
type challenge struct {
	path    string
	content string
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && p.challenge.path != "" && r.URL.Path == p.challenge.path {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, p.challenge.content+"\n")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	verdict, err := p.verifier.Verify(r, body, time.Now())
	if err != nil {
		// No detail in the body: a reason tells a prober how close they are.
		log.Printf("refused: %v", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// The proof goes on EVERY answer to a verified request, errors included,
	// and BEFORE WriteHeader: a header set afterwards is dropped in silence.
	w.Header().Set("X-Nexus-Proof", verdict.Proof)
	w.Header().Set("Content-Type", "application/json")
	if verdict.Hello {
		w.WriteHeader(http.StatusOK)
		return
	}

	var call struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &call); err != nil || call.Method != "tools/call" {
		answer(w, http.StatusBadRequest, "the body is not a tools/call request")
		return
	}
	name, params, args, err := renameTool(call.Params)
	if err != nil {
		answer(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	offered, known, schema := p.tools.lookup(ctx, name)
	if known && !offered {
		answer(w, http.StatusNotFound, "unknown tool "+name)
		return
	}
	// Arguments the tool's own schema rules out never reach it: nothing is
	// processed, so the caller is refunded (ADR-0234, amended 2026-11-18).
	if reason := checkArguments(schema, args); reason != "" {
		answer(w, http.StatusBadRequest, "the arguments do not fit the input schema of "+name+": "+reason)
		return
	}
	result, err := p.up.call(ctx, "tools/call", params)
	if err != nil {
		status, text := failure(ctx, err)
		log.Printf("tools/call %s: HTTP %d (%s)", name, status, errorClass(err))
		answer(w, status, text)
		return
	}
	if len(bytes.TrimSpace(result)) == 0 || bytes.Equal(bytes.TrimSpace(result), []byte("null")) {
		answer(w, http.StatusBadGateway, "the MCP server answered with no result")
		return
	}
	// A tool that ran and FAILED is not the caller's to pay for (operator,
	// 2026-11-18): a 502, refunded, unless the server marks the result as the
	// caller's fault after it did work, `_meta: {"nexus402/fault": "client"}`.
	if failed, text := toolFailed(result); failed {
		log.Printf("tools/call %s: HTTP 502 (the tool reported a failure)", name)
		answer(w, http.StatusBadGateway, text)
		return
	}
	// The result exactly as the upstream wrote it; the gateway adds the
	// JSON-RPC envelope, so passing the upstream's on would nest it.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result)
}

// clientFault is the _meta key a server sets to "client" on a failed result
// when the CALLER's input made the tool fail after it did work: that call is
// charged.
const clientFault = "nexus402/fault"

// toolFailed reports an `isError: true` result that is not marked as the
// caller's fault, with the result's first text for the caller to read.
func toolFailed(result json.RawMessage) (bool, string) {
	var r struct {
		IsError bool                       `json:"isError"`
		Meta    map[string]json.RawMessage `json:"_meta"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(result, &r) != nil || !r.IsError {
		return false, ""
	}
	var fault string
	if json.Unmarshal(r.Meta[clientFault], &fault) == nil && fault == "client" {
		return false, ""
	}
	text := "the tool failed"
	for _, c := range r.Content {
		if c.Type == "text" && c.Text != "" {
			text = c.Text
			break
		}
	}
	if len(text) > 300 {
		text = text[:300]
	}
	return true, text
}

// renameTool takes the gateway's params and returns the upstream tool name
// (the last segment of the published `creator/server@v/tool`), the params
// with ONLY name replaced, and the arguments. Every other field, the
// arguments included, is copied byte for byte.
func renameTool(params json.RawMessage) (string, json.RawMessage, json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(params, &fields); err != nil || fields == nil {
		return "", nil, nil, errors.New("params is not an object")
	}
	var published string
	if err := json.Unmarshal(fields["name"], &published); err != nil || published == "" {
		return "", nil, nil, errors.New("params.name is not a tool name")
	}
	name := published[strings.LastIndex(published, "/")+1:]
	if name == "" {
		return "", nil, nil, errors.New("params.name has no tool segment")
	}
	quoted, err := json.Marshal(name)
	if err != nil {
		return "", nil, nil, errors.New("params.name is not a tool name")
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		if key != "name" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteString(`{"name":`)
	b.Write(quoted)
	for _, key := range keys {
		k, _ := json.Marshal(key) // a string always marshals
		b.WriteByte(',')
		b.Write(k)
		b.WriteByte(':')
		b.Write(fields[key])
	}
	b.WriteByte('}')
	return name, b.Bytes(), fields["arguments"], nil
}

// failure maps an upstream failure to the status the gateway acts on. A 4xx
// is the CALLER's mistake: refunded, and not held against the endpoint. A
// 5xx is the server's: refunded, and counted against its health.
func failure(ctx context.Context, err error) (int, string) {
	var upstreamErr *rpcError
	if errors.As(err, &upstreamErr) {
		text := upstreamErr.Message
		if len(text) > 300 {
			text = text[:300]
		}
		switch upstreamErr.Code {
		case codeMethodNotFound:
			return http.StatusNotFound, text
		case codeInvalidParams, codeInvalidRequest, codeParse:
			return http.StatusBadRequest, text
		default:
			return http.StatusBadGateway, text
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "the MCP server did not answer in time"
	}
	return http.StatusBadGateway, "the MCP server behind this endpoint did not answer"
}

// errorClass names a failure for the log without its text, which may echo
// what the caller sent.
func errorClass(err error) string {
	var upstreamErr *rpcError
	if errors.As(err, &upstreamErr) {
		return "JSON-RPC error " + strconv.Itoa(upstreamErr.Code)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "unreachable: " + err.Error()
}

func answer(w http.ResponseWriter, status int, text string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": text})
}
