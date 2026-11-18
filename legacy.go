package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// readEvents reads a server-sent event stream and calls fn with each
// event's type and data, until fn returns false or the stream ends.
func readEvents(r io.Reader, fn func(event, data string) bool) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxMessageBytes)
	var event string
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if data.Len() > 0 {
				if !fn(event, strings.TrimSuffix(data.String(), "\n")) {
					return nil
				}
			}
			event = ""
			data.Reset()
		case strings.HasPrefix(line, ":"):
			// a comment, often a keep-alive
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				event = value
			case "data":
				data.WriteString(value)
				data.WriteByte('\n')
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("event stream: %w", err)
	}
	if data.Len() > 0 {
		fn(event, strings.TrimSuffix(data.String(), "\n"))
	}
	return nil
}

// legacyUpstream speaks the older HTTP+SSE transport (MCP 2024-11-05): a GET
// opens an event stream whose first `endpoint` event names where to POST;
// every answer then arrives on that stream. Many servers still offer only
// this. A dropped stream is reopened on the next call.
type legacyUpstream struct {
	client   *http.Client
	url      string
	onNotify func(string)

	session *streamSession
	post    atomic.Pointer[string] // set from the endpoint event, before any send
	stop    context.CancelFunc
}

func dialLegacy(ctx context.Context, client *http.Client, base string, onNotify func(string)) (*legacyUpstream, error) {
	streamCtx, stop := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(streamCtx, http.MethodGet, base, nil)
	if err != nil {
		stop()
		return nil, unreachable("event stream: %v", err)
	}
	request.Header.Set("Accept", "text/event-stream")
	response, err := client.Do(request)
	if err != nil {
		stop()
		return nil, unreachable("event stream: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		drain(response)
		stop()
		return nil, unreachable("event stream: HTTP %d; the upstream speaks neither MCP HTTP transport", response.StatusCode)
	}
	u := &legacyUpstream{client: client, url: base, onNotify: onNotify, stop: stop}
	endpoint := make(chan string, 1)
	u.session = newStreamSession(u.send, onNotify)
	go func() {
		defer func() { _ = response.Body.Close() }()
		err := readEvents(response.Body, func(event, data string) bool {
			if event == "endpoint" {
				select {
				case endpoint <- data:
				default:
				}
				return true
			}
			var m message
			if json.Unmarshal([]byte(data), &m) == nil {
				u.session.deliver(m)
			}
			return true
		})
		u.session.end(errOr(err, errors.New("the event stream closed")))
	}()
	select {
	case raw := <-endpoint:
		resolved, err := resolveEndpoint(base, raw)
		if err != nil {
			u.close()
			return nil, err
		}
		u.post.Store(&resolved)
	case <-ctx.Done():
		u.close()
		return nil, unreachable("no endpoint event: %v", ctx.Err())
	}
	if _, err := u.session.request(ctx, "initialize", initializeParams()); err != nil {
		u.close()
		return nil, unreachable("initialize: %v", err)
	}
	if err := u.session.notify("notifications/initialized", nil); err != nil {
		u.close()
		return nil, unreachable("initialized: %v", err)
	}
	log.Printf("connected to the MCP server over HTTP+SSE")
	return u, nil
}

// resolveEndpoint resolves the endpoint event against the stream's URL and
// refuses one on another origin: the proxy posts only to its own upstream.
func resolveEndpoint(base, raw string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", unreachable("upstream URL: %v", err)
	}
	ref, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", unreachable("endpoint event: %v", err)
	}
	resolved := b.ResolveReference(ref)
	if resolved.Scheme != b.Scheme || resolved.Host != b.Host {
		return "", unreachable("the endpoint event names another origin (%s)", resolved.Host)
	}
	return resolved.String(), nil
}

func (u *legacyUpstream) send(body []byte) error {
	post := u.post.Load()
	if post == nil {
		return errors.New("no endpoint event yet")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, *post, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := u.client.Do(request)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	drain(response)
	if response.StatusCode >= 300 {
		return statusError{response.StatusCode}
	}
	return nil
}

func (u *legacyUpstream) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return u.session.request(ctx, method, params)
}

func (u *legacyUpstream) alive() bool { return u.session.alive() }

func (u *legacyUpstream) close() {
	u.stop()
	u.session.end(errors.New("closed"))
}
