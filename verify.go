package main

// The request verifier, IDENTICAL from `import` down to the Go starter kit's
// verify.go (examples/verifiers/go), which the gateway's real signer tests;
// a test fails if the two ever differ. Standard library only.
//
// Two checks, and you want both. The creator key is a shared secret on your
// server; if it leaks, anyone can impersonate Nexus402 to you. The signature
// is asymmetric (you hold only a public key), and it binds a request to YOUR
// host and to a 60-second window, so it also stops replays.

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ProofDomain is the fixed first line of the response proof's digested bytes.
const ProofDomain = "nexus-endpoint-proof-v1"

// SignatureWindow must match the gateway's 60 seconds, in both directions.
const SignatureWindow = 60 * time.Second

// Verdict is what a verified request concluded.
type Verdict struct {
	// Hello is the signed, empty-body check the gateway sends when you
	// publish: answer it 200 with the proof and run no tool.
	Hello bool
	// Proof goes in X-Nexus-Proof on every answer to this request.
	Proof string
}

// Verifier holds what you configured.
type Verifier struct {
	RegisteredHost string
	CreatorKey     string
	PublicKeys     []ed25519.PublicKey
}

// ParsePublicKeys reads the gateway's key list: base64, comma-separated.
func ParsePublicKeys(raw string) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	for _, part := range strings.Split(raw, ",") {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(part))
		if err != nil || len(decoded) != ed25519.PublicKeySize {
			return nil, errors.New("NEXUS_PUBLIC_KEY must be base64 Ed25519 keys of 32 bytes, comma-separated")
		}
		keys = append(keys, ed25519.PublicKey(decoded))
	}
	return keys, nil
}

// CanonicalString is the signed message: six fields joined by single
// newlines, in this order, no trailing newline. host is the host you
// PUBLISHED, never the inbound Host header, which a proxy may rewrite.
func CanonicalString(method, host, pathWithQuery, timestamp, correlationID, bodySHA256Hex string) string {
	// nexus:canonical-begin
	return strings.Join([]string{method, host, pathWithQuery, timestamp, correlationID, bodySHA256Hex}, "\n")
	// nexus:canonical-end
}

// Proof is the value Nexus402 requires on every answer to a verified request.
func Proof(correlationID, timestamp, signature string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{ProofDomain, correlationID, timestamp, signature}, "\n")))
	return "sha256:" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// RequestTarget is the path with the query, exactly as sent.
func RequestTarget(r *http.Request) string {
	return r.URL.RequestURI()
}

// Verify checks one request whose raw body has already been read.
func (v Verifier) Verify(r *http.Request, body []byte, now time.Time) (Verdict, error) {
	hello := len(body) == 0

	// Constant time: a byte-at-a-time compare leaks the key one position at
	// a time. The hello carries no key, so it skips only this check.
	presented := sha256.Sum256([]byte(r.Header.Get("X-Nexus-Creator-Key")))
	expected := sha256.Sum256([]byte(v.CreatorKey))
	if !hello && subtle.ConstantTimeCompare(presented[:], expected[:]) != 1 {
		return Verdict{}, errors.New("creator key mismatch")
	}

	timestamp := r.Header.Get("X-Nexus-Timestamp")
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return Verdict{}, errors.New("timestamp is not an integer")
	}
	// BOTH directions: no upper bound means a future-dated request is valid
	// forever; no lower bound means a captured one replays forever.
	if skew := now.Sub(time.Unix(seconds, 0)); skew > SignatureWindow || skew < -SignatureWindow {
		return Verdict{}, errors.New("timestamp outside the 60-second window")
	}

	header := r.Header.Get("X-Nexus-Signature")
	encoded, found := strings.CutPrefix(header, "ed25519:")
	if !found {
		return Verdict{}, errors.New("signature lacks the ed25519 prefix")
	}
	signature, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return Verdict{}, errors.New("signature is not 64 bytes of base64")
	}

	correlationID := r.Header.Get("X-Correlation-ID")
	sum := sha256.Sum256(body)
	message := []byte(CanonicalString(r.Method, v.RegisteredHost, RequestTarget(r), timestamp, correlationID, hex.EncodeToString(sum[:])))
	// More than one key while the gateway rotates: accept any of them.
	for _, key := range v.PublicKeys {
		if ed25519.Verify(key, message, signature) {
			return Verdict{Hello: hello, Proof: Proof(correlationID, timestamp, header)}, nil
		}
	}
	return Verdict{}, errors.New("signature does not verify")
}
