package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"
)

// stdioUpstream runs an MCP server that speaks over stdin/stdout as a child
// process: one JSON-RPC message per line each way. It is started on first
// use, initialized once, shared by concurrent calls (each has its own id),
// and restarted after it exits, at most once a second.
type stdioUpstream struct {
	argv     []string
	onNotify func(string)

	mu        sync.Mutex
	proc      *stdioProcess
	lastStart time.Time
}

type stdioProcess struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	session *streamSession
}

const restartGap = time.Second

func newStdioUpstream(argv []string, onNotify func(string)) *stdioUpstream {
	return &stdioUpstream{argv: argv, onNotify: onNotify}
}

func (u *stdioUpstream) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	session, err := u.session(ctx)
	if err != nil {
		return nil, err
	}
	return session.request(ctx, method, params)
}

func (u *stdioUpstream) session(ctx context.Context) (*streamSession, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.proc != nil && u.proc.session.alive() {
		return u.proc.session, nil
	}
	if wait := restartGap - time.Since(u.lastStart); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, unreachable("waiting to restart the server: %v", ctx.Err())
		}
	}
	u.lastStart = time.Now()
	proc, err := u.start()
	if err != nil {
		return nil, err
	}
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := proc.session.request(initCtx, "initialize", initializeParams()); err != nil {
		proc.stop()
		return nil, unreachable("initialize: %v", err)
	}
	if err := proc.session.notify("notifications/initialized", nil); err != nil {
		proc.stop()
		return nil, unreachable("initialized: %v", err)
	}
	u.proc = proc
	return proc.session, nil
}

func (u *stdioUpstream) start() (*stdioProcess, error) {
	// The command is the creator's own server, named on THIS proxy's command
	// line by whoever runs it; running it is this mode's whole purpose, and
	// nothing a caller sends reaches it. Resolved as exec.Command resolves it.
	path, err := exec.LookPath(u.argv[0])
	if err != nil {
		return nil, unreachable("find %s: %v", u.argv[0], err)
	}
	cmd := &exec.Cmd{Path: path, Args: u.argv, Stderr: os.Stderr}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, unreachable("stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, unreachable("stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, unreachable("start %s: %v", u.argv[0], err)
	}
	proc := &stdioProcess{cmd: cmd, stdin: stdin}
	proc.session = newStreamSession(proc.write, u.onNotify)
	go proc.read(stdout)
	log.Printf("started the MCP server: %s (pid %d)", u.argv[0], cmd.Process.Pid)
	return proc, nil
}

func (p *stdioProcess) write(line []byte) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if _, err := p.stdin.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write to the server: %w", err)
	}
	return nil
}

// read delivers each line the server writes, until it closes stdout or
// writes a line over the cap; then the process is ended and reaped.
func (p *stdioProcess) read(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maxMessageBytes)
	for scanner.Scan() {
		var m message
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			// MCP forbids anything but messages on stdout; a server that logs
			// there is told so on stderr rather than taken down for it.
			log.Printf("ignored a non-JSON line from the server's stdout (%d bytes); servers must log to stderr", len(scanner.Bytes()))
			continue
		}
		p.session.deliver(m)
	}
	reason := scanner.Err()
	if reason == nil {
		reason = errors.New("the server closed stdout")
	}
	p.session.end(reason)
	_ = p.cmd.Process.Kill()
	err := p.cmd.Wait()
	log.Printf("the MCP server exited (%v); it restarts on the next call", errOr(err, reason))
}

func (p *stdioProcess) stop() {
	_ = p.stdin.Close()
	_ = p.cmd.Process.Kill()
	p.session.end(errors.New("stopped"))
}

func (u *stdioUpstream) close() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.proc != nil {
		u.proc.stop()
	}
}

func errOr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}
