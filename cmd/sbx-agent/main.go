// Command sbx-agent is the in-sandbox agent: the static daemon that runs
// as each sandbox's long-lived process. See package agent.
//
// Build it fully static so it can be bind-mounted into any image:
//
//	CGO_ENABLED=0 go build ./cmd/sbx-agent
package main

import (
	"os"

	"github.com/himanshubaliyan7/warmbox/internal/agent"
)

func main() {
	os.Exit(agent.Main(os.Args[1:], os.Stderr))
}
