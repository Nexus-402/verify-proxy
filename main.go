// Command verify-proxy puts an EXISTING MCP server on Nexus402 without
// changing its code (ADR-0234). It runs beside the server, is the public
// HTTPS endpoint you publish, admits only calls the gateway signed and
// charged, and forwards each to your server with only the tool name changed.
//
//	verify-proxy                     serve, upstream from UPSTREAM_URL
//	verify-proxy -- <command> [args] serve, starting a stdio MCP server
//	verify-proxy tools [-- <command>] print your tools, ready for the publish form
//	verify-proxy version
//
// Environment:
//
//	UPSTREAM_URL             your MCP server over HTTP (Streamable HTTP, or the
//	                         older HTTP+SSE), on a PRIVATE address, e.g.
//	                         http://127.0.0.1:3000/mcp
//	PORT                     where to listen (default 8080)
//	NEXUS_REGISTERED_HOST    the host you published, e.g. mcp.acme.example
//	NEXUS_CREATOR_KEY        your nx_crt_ key, shown once when you claimed a handle
//	NEXUS_PUBLIC_KEY         the gateway's Ed25519 key(s), base64, comma-separated
//	                         (GET https://api.nexus402.dev/v1/signing-key lists them)
//	NEXUS_CHALLENGE_URL      optional, with NEXUS_CHALLENGE_CONTENT: the host
//	NEXUS_CHALLENGE_CONTENT  proof a first publish at a new host asks for
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// version is this release; VERSION holds the same, and a test keeps them equal.
const version = "1.0.0"

const challengePrefix = "/.well-known/nexus402-challenge/"

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	command, argv, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, "usage: verify-proxy [tools | version] [-- <stdio server command>]")
		os.Exit(2)
	}
	if command == "version" {
		fmt.Println("verify-proxy", version)
		return
	}
	tools := &toolNames{}
	up, err := newUpstream(os.Getenv("UPSTREAM_URL"), argv, tools.notified)
	if err != nil {
		log.Fatal(err)
	}
	tools.up = up
	if command == "tools" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		err := printTools(ctx, up, os.Stdout)
		cancel()
		up.close()
		if err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := serve(up, tools); err != nil {
		log.Fatal(err)
	}
}

// parseArgs splits `[command] [-- server command...]`.
func parseArgs(args []string) (string, []string, error) {
	var argv []string
	for i, arg := range args {
		if arg == "--" {
			argv = args[i+1:]
			args = args[:i]
			break
		}
	}
	switch {
	case len(args) == 0:
		return "serve", argv, nil
	case len(args) == 1 && (args[0] == "serve" || args[0] == "tools" || args[0] == "version"):
		return args[0], argv, nil
	default:
		return "", nil, fmt.Errorf("unknown arguments: %s", strings.Join(args, " "))
	}
}

// newUpstream is UPSTREAM_URL or a stdio command, exactly one of them.
func newUpstream(rawURL string, argv []string, onNotify func(string)) (upstream, error) {
	rawURL = strings.TrimSpace(rawURL)
	switch {
	case rawURL != "" && len(argv) > 0:
		return nil, errors.New("set UPSTREAM_URL or give a command after --, not both")
	case len(argv) > 0:
		return newStdioUpstream(argv, onNotify), nil
	case rawURL == "":
		return nil, errors.New("set UPSTREAM_URL to your MCP server, or give its stdio command after --")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("UPSTREAM_URL must be an http:// or https:// URL")
	}
	return newHTTPUpstream(parsed.String(), onNotify), nil
}

func serve(up upstream, tools *toolNames) error {
	keys, err := ParsePublicKeys(required("NEXUS_PUBLIC_KEY"))
	if err != nil {
		return err
	}
	ch, err := challengeFromEnv(os.Getenv("NEXUS_CHALLENGE_URL"), os.Getenv("NEXUS_CHALLENGE_CONTENT"))
	if err != nil {
		return err
	}
	handler := &proxy{
		verifier: Verifier{
			RegisteredHost: required("NEXUS_REGISTERED_HOST"),
			CreatorKey:     required("NEXUS_CREATOR_KEY"),
			PublicKeys:     keys,
		},
		up:        up,
		tools:     tools,
		challenge: ch,
	}
	port := 8080
	if raw := os.Getenv("PORT"); raw != "" {
		port, err = strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return errors.New("PORT must be a number between 1 and 65535")
		}
	}
	server := &http.Server{
		Addr:              ":" + strconv.Itoa(port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      callTimeout + 10*time.Second,
	}
	// Reach the upstream once now, so a wrong address shows at start and a
	// stdio server is warm before the first call. Not fatal: the server may
	// start after the proxy.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := listTools(ctx, up); err != nil {
			log.Printf("the MCP server is not reachable yet: %v", err)
		} else {
			log.Print("the MCP server answered tools/list; `verify-proxy tools` prints them")
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	failed := make(chan error, 1)
	go func() {
		log.Printf("verify-proxy %s listening on :%d", version, port)
		failed <- server.ListenAndServe()
	}()
	select {
	case err := <-failed:
		up.close()
		return err
	case <-stop:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = server.Shutdown(ctx)
	up.close()
	return err
}

// challengeFromEnv reads the host proof: the challenge_url and
// challenge_content a publish's `host_proof_required` refusal names.
func challengeFromEnv(rawURL, content string) (challenge, error) {
	rawURL, content = strings.TrimSpace(rawURL), strings.TrimSpace(content)
	if rawURL == "" && content == "" {
		return challenge{}, nil
	}
	if rawURL == "" || content == "" {
		return challenge{}, errors.New("set both NEXUS_CHALLENGE_URL and NEXUS_CHALLENGE_CONTENT, or neither")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || !strings.HasPrefix(parsed.Path, challengePrefix) || len(parsed.Path) == len(challengePrefix) {
		return challenge{}, errors.New("NEXUS_CHALLENGE_URL must be the challenge_url the publish refusal gave")
	}
	return challenge{path: parsed.Path, content: content}, nil
}

func required(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		log.Fatalf("%s is not set", name)
	}
	return value
}
