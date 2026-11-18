package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// httpUpstream speaks MCP's Streamable HTTP transport to an upstream URL:
// initialize once, carry the session id and the negotiated version on every
// request, take the answer as JSON or as an event stream, and initialize
// again when the upstream forgets the session (404). An upstream that refuses
// the initialize POST with a 4xx is tried with the older HTTP+SSE transport
// (legacy.go), as the MCP specification's compatibility rule says.
type httpUpstream struct {
	url      string
	client   *http.Client
	onNotify func(string)

	nextID atomic.Int64
	mu     sync.Mutex
	ready  bool
	sid    string // Mcp-Session-Id, empty for a stateless upstream
	ver    string // the negotiated protocol version
	legacy *legacyUpstream
}

func newHTTPUpstream(url string, onNotify func(string)) *httpUpstream {
	return &httpUpstream{url: url, client: &http.Client{}, onNotify: onNotify}
}

// statusError is an HTTP answer that carried no JSON-RPC message.
type statusError struct{ code int }

func (e statusError) Error() string { return "upstream answered HTTP " + strconv.Itoa(e.code) }

func (u *httpUpstream) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		legacy, sid, ver, err := u.ensure(ctx)
		if err != nil {
			return nil, err
		}
		if legacy != nil {
			return legacy.call(ctx, method, params)
		}
		id := u.nextID.Add(1)
		body, err := encodeRequest(id, method, params)
		if err != nil {
			return nil, err
		}
		m, err := u.post(ctx, body, strconv.FormatInt(id, 10), sid, ver)
		var status statusError
		if errors.As(err, &status) && status.code == http.StatusNotFound && sid != "" && attempt == 0 {
			// The upstream ended the session (a restart, an expiry): start a
			// new one and send the call once more.
			u.reset(sid)
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				u.cancelled(id, sid, ver)
			}
			return nil, unreachableOr(err)
		}
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	}
}

// cancelled tells the upstream a request's caller went away. A dropped
// connection alone does not cancel a request in MCP; this notification does.
func (u *httpUpstream) cancelled(id int64, sid, ver string) {
	note, err := encodeNotification("notifications/cancelled", map[string]any{"requestId": id, "reason": "the caller went away"})
	if err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = u.post(ctx, note, "", sid, ver)
	}()
}

// ensure initializes the upstream once, and again after a reset.
func (u *httpUpstream) ensure(ctx context.Context) (*legacyUpstream, string, string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.legacy != nil {
		if u.legacy.alive() {
			return u.legacy, "", "", nil
		}
		legacy, err := dialLegacy(ctx, u.client, u.url, u.onNotify)
		if err != nil {
			return nil, "", "", err
		}
		u.legacy = legacy
		return legacy, "", "", nil
	}
	if u.ready {
		return nil, u.sid, u.ver, nil
	}
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	id := u.nextID.Add(1)
	body, err := encodeRequest(id, "initialize", initializeParams())
	if err != nil {
		return nil, "", "", err
	}
	request, err := u.newRequest(initCtx, http.MethodPost, body, "", "")
	if err != nil {
		return nil, "", "", err
	}
	response, err := u.client.Do(request)
	if err != nil {
		return nil, "", "", unreachable("initialize: %v", err)
	}
	if response.StatusCode >= 400 && response.StatusCode < 500 {
		drain(response)
		log.Printf("the upstream refused Streamable HTTP (HTTP %d); trying the older HTTP+SSE transport", response.StatusCode)
		legacy, err := dialLegacy(initCtx, u.client, u.url, u.onNotify)
		if err != nil {
			return nil, "", "", err
		}
		u.legacy = legacy
		return legacy, "", "", nil
	}
	m, err := u.readAnswer(initCtx, response, strconv.FormatInt(id, 10), "", "")
	if err != nil {
		return nil, "", "", unreachableOr(err)
	}
	if m.Error != nil {
		return nil, "", "", unreachable("initialize: %v", m.Error)
	}
	u.sid, u.ver, u.ready = response.Header.Get("Mcp-Session-Id"), negotiated(m.Result), true
	if note, err := encodeNotification("notifications/initialized", nil); err == nil {
		if _, err := u.post(initCtx, note, "", u.sid, u.ver); err != nil {
			log.Printf("notifications/initialized: %v", err)
		}
	}
	log.Printf("connected to the MCP server over Streamable HTTP (protocol %s, session %t)", u.ver, u.sid != "")
	return nil, u.sid, u.ver, nil
}

// reset forgets the session sid, unless another call already replaced it.
func (u *httpUpstream) reset(sid string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.sid == sid {
		u.ready, u.sid = false, ""
	}
}

func (u *httpUpstream) newRequest(ctx context.Context, method string, body []byte, sid, ver string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, u.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if sid != "" {
		request.Header.Set("Mcp-Session-Id", sid)
	}
	if ver != "" {
		request.Header.Set("MCP-Protocol-Version", ver)
	}
	return request, nil
}

// post sends one message. With want empty (a notification or a reply) any
// 2xx is enough; otherwise the answer with id want is returned.
func (u *httpUpstream) post(ctx context.Context, body []byte, want, sid, ver string) (message, error) {
	request, err := u.newRequest(ctx, http.MethodPost, body, sid, ver)
	if err != nil {
		return message{}, err
	}
	response, err := u.client.Do(request)
	if err != nil {
		return message{}, unreachable("%v", err)
	}
	if want == "" {
		drain(response)
		if response.StatusCode >= 300 {
			return message{}, statusError{response.StatusCode}
		}
		return message{}, nil
	}
	return u.readAnswer(ctx, response, want, sid, ver)
}

// readAnswer reads the answer to request want from a JSON body or an event
// stream. On a stream, the upstream's own requests are answered and its
// notifications noted until the answer arrives.
func (u *httpUpstream) readAnswer(ctx context.Context, response *http.Response, want, sid, ver string) (message, error) {
	defer drain(response)
	if response.StatusCode >= 300 {
		return message{}, statusError{response.StatusCode}
	}
	media, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	body := io.LimitReader(response.Body, maxMessageBytes+1)
	if media == "text/event-stream" {
		var answer *message
		err := readEvents(body, func(_, data string) bool {
			var m message
			if json.Unmarshal([]byte(data), &m) != nil {
				return true
			}
			switch {
			case m.isResponse() && string(m.ID) == want:
				answer = &m
				return false
			case m.isRequest():
				go func() { _, _ = u.post(context.WithoutCancel(ctx), answerServerRequest(m), "", sid, ver) }()
			case m.Method != "" && u.onNotify != nil:
				u.onNotify(m.Method)
			}
			return true
		})
		if answer != nil {
			return *answer, nil
		}
		return message{}, unreachable("the event stream ended without an answer: %v", errOr(err, io.ErrUnexpectedEOF))
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		return message{}, unreachable("read: %v", err)
	}
	if len(raw) > maxMessageBytes {
		return message{}, unreachable("the answer is over %d bytes", maxMessageBytes)
	}
	var batch []message
	if json.Unmarshal(raw, &batch) != nil {
		var single message
		if err := json.Unmarshal(raw, &single); err != nil {
			return message{}, unreachable("the answer is not JSON-RPC: %v", err)
		}
		batch = []message{single}
	}
	for _, m := range batch {
		if m.isResponse() && string(m.ID) == want {
			return m, nil
		}
	}
	return message{}, unreachable("the answer did not carry id %s", want)
}

// close ends the session upstream, as the specification asks a client to.
func (u *httpUpstream) close() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.legacy != nil {
		u.legacy.close()
		return
	}
	if u.sid == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if request, err := u.newRequest(ctx, http.MethodDelete, nil, u.sid, u.ver); err == nil {
		if response, err := u.client.Do(request); err == nil {
			drain(response)
		}
	}
}

func drain(response *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	_ = response.Body.Close()
}

func unreachableOr(err error) error {
	if errors.Is(err, errUnreachable) {
		return err
	}
	return unreachable("%v", err)
}
