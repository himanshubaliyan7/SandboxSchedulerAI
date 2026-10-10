package webhook

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Interoperability with the Standard Webhooks reference library for
// JavaScript, in both directions: what this package signs, the library
// verifies; what the library signs, this package verifies.
//
// It needs Node and the npm package "standardwebhooks", which is not a
// dependency of this repository, so it is gated: install the package
// somewhere and point SBX_STANDARDWEBHOOKS_DIR at the directory that
// holds its node_modules.
//
//	(cd /tmp/sw && npm install standardwebhooks)
//	SBX_STANDARDWEBHOOKS_DIR=/tmp/sw go test -run TestInteropWithReferenceLibrary -v ./internal/webhook/
func TestInteropWithReferenceLibrary(t *testing.T) {
	dir := os.Getenv("SBX_STANDARDWEBHOOKS_DIR")
	if dir == "" {
		t.Skip("set SBX_STANDARDWEBHOOKS_DIR to a directory with the npm package standardwebhooks installed")
	}
	secret, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	env, _ := Translate(webhookEventForInterop())
	body, err := env.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	h, err := signatureHeaders(env.ID, now, body, secret)
	if err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(map[string]any{
		"secret": secret, "body": string(body),
		"headers": map[string]string{"webhook-id": h.Get(HeaderID), "webhook-timestamp": h.Get(HeaderTimestamp), "webhook-signature": h.Get(HeaderSignature)},
	})
	const script = `
const { Webhook } = require('standardwebhooks')
const input = JSON.parse(require('fs').readFileSync(0, 'utf8'))
const wh = new Webhook(input.secret)
const verified = wh.verify(input.body, input.headers)           // throws unless authentic
let tampered = false
try { wh.verify(input.body + ' ', input.headers) } catch { tampered = true }
const at = new Date()
const theirs = '{"id":"evt_ref","type":"sandbox.created","version":1,"data":{}}'
process.stdout.write(JSON.stringify({
  verifiedType: verified.type, tampered,
  id: 'evt_ref', timestamp: String(Math.floor(at.getTime() / 1000)), body: theirs,
  signature: wh.sign('evt_ref', at, theirs),
}))`
	cmd := exec.Command("node", "-e", script)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(string(in))
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("the reference library refused a request signed by this package: %v\n%s", err, stderr)
	}
	var res struct {
		VerifiedType string `json:"verifiedType"`
		Tampered     bool   `json:"tampered"`
		ID           string `json:"id"`
		Timestamp    string `json:"timestamp"`
		Body         string `json:"body"`
		Signature    string `json:"signature"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("node answered %q: %v", out, err)
	}
	if res.VerifiedType != TypeSandboxTerminated || !res.Tampered {
		t.Fatalf("reference verify: %+v", res)
	}
	theirs := http.Header{}
	theirs.Set(HeaderID, res.ID)
	theirs.Set(HeaderTimestamp, res.Timestamp)
	theirs.Set(HeaderSignature, res.Signature)
	if err := Verify(secret, theirs, []byte(res.Body), time.Now(), 0); err != nil {
		t.Fatalf("this package refused a request signed by the reference library: %v", err)
	}
	if err := Verify(secret, theirs, []byte(res.Body+" "), time.Now(), 0); err == nil {
		t.Fatal("a tampered reference request verified")
	}
}
