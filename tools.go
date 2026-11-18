package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// upstream is the creator's MCP server, however it is reached.
type upstream interface {
	call(ctx context.Context, method string, params any) (json.RawMessage, error)
	close()
}

// tool is one entry of the upstream's tools/list.
type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// listTools reads every page of the upstream's tools/list.
func listTools(ctx context.Context, up upstream) ([]tool, error) {
	var all []tool
	cursor := ""
	for page := 0; page < 100; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := up.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var result struct {
			Tools      []tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, unreachable("tools/list is not a tool list: %v", err)
		}
		all = append(all, result.Tools...)
		if result.NextCursor == "" {
			return all, nil
		}
		cursor = result.NextCursor
	}
	return all, nil
}

// refreshGap is how often a name the proxy has not seen may trigger a fresh
// tools/list, so a stream of unknown names cannot flood the upstream.
const refreshGap = 30 * time.Second

// toolNames knows which tools the upstream offers, so a call to one it does
// not offer is answered 404 without reaching it, whatever error shape the
// upstream's SDK would have used. It refreshes on the upstream's
// tools/list_changed notification and, at most every refreshGap, on a name
// it has not seen.
type toolNames struct {
	up      upstream
	mu      sync.Mutex
	names   map[string]json.RawMessage // each offered tool's input schema
	fetched time.Time
	stale   bool
}

// notified is the upstreams' notification hook.
func (t *toolNames) notified(method string) {
	if method == "notifications/tools/list_changed" {
		t.mu.Lock()
		t.stale = true
		t.mu.Unlock()
	}
}

// lookup reports whether the upstream offers name and that tool's input
// schema, and whether the answer is known at all: when tools/list has never
// succeeded, the call goes ahead and the upstream's own answer decides.
func (t *toolNames) lookup(ctx context.Context, name string) (offered, known bool, schema json.RawMessage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.names != nil && !t.stale {
		if schema, ok := t.names[name]; ok || time.Since(t.fetched) < refreshGap {
			return ok, true, schema
		}
	}
	listCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tools, err := listTools(listCtx, t.up)
	if err != nil {
		schema, ok := t.names[name]
		return ok, t.names != nil, schema
	}
	t.names = map[string]json.RawMessage{}
	for _, tl := range tools {
		t.names[tl.Name] = tl.InputSchema
	}
	t.fetched, t.stale = time.Now(), false
	schema, ok := t.names[name]
	return ok, true, schema
}

// printTools is `verify-proxy tools`: each tool with the description and
// input schema to paste into the publish form, as JSON.
func printTools(ctx context.Context, up upstream, out io.Writer) error {
	tools, err := listTools(ctx, up)
	if err != nil {
		return fmt.Errorf("tools/list: %w", err)
	}
	type entry struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	entries := make([]entry, 0, len(tools))
	for _, tl := range tools {
		schema := tl.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		entries = append(entries, entry{Name: tl.Name, Description: tl.Description, InputSchema: schema})
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false) // `<` stays `<`: this is pasted by a person, not served
	if err := encoder.Encode(entries); err != nil {
		return fmt.Errorf("print: %w", err)
	}
	return nil
}
