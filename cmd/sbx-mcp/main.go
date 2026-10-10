// Command sbx-mcp is a Model Context Protocol server (stdio transport)
// that lets an MCP client such as Claude Code, Claude Desktop or Cursor
// run code in sbxd sandboxes.
//
//	SBX_BASE_URL=http://127.0.0.1:8080 SBX_API_KEY=sbx_... sbx-mcp --template python
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/mcp"
)

func main() {
	fs := flag.NewFlagSet("sbx-mcp", flag.ExitOnError)
	baseURL := fs.String("base-url", envOr("SBX_BASE_URL", "http://127.0.0.1:8080"), "sbxd address (env SBX_BASE_URL)")
	template := fs.String("template", os.Getenv("SBX_DEFAULT_TEMPLATE"), "template used when create_sandbox names none (env SBX_DEFAULT_TEMPLATE)")
	maxOutput := fs.Int("max-output-bytes", 32<<10, "cap on each of stdout and stderr in a tool result")
	maxFile := fs.Int("max-file-bytes", 256<<10, "cap on what read_file returns")
	version := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: sbx-mcp [flags]\n\nServes MCP on stdin/stdout. The API key is read from SBX_API_KEY only,\nso that it does not appear in the process list.\n\nFlags:")
		fs.PrintDefaults()
	}
	_ = fs.Parse(os.Args[1:])
	if *version {
		fmt.Println(mcp.Version)
		return
	}

	// stdout carries the protocol; everything else goes to stderr.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	apiKey := os.Getenv("SBX_API_KEY")
	if apiKey == "" {
		log.Error("SBX_API_KEY is not set")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Serve blocks reading stdin, which a signal does not interrupt.
	go func() {
		<-ctx.Done()
		os.Exit(0)
	}()
	srv := mcp.NewServer(mcp.NewClient(*baseURL, apiKey), mcp.Config{
		DefaultTemplate: *template,
		MaxOutputBytes:  *maxOutput,
		MaxFileBytes:    *maxFile,
		Logger:          log,
	})
	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		log.Error("stopped", "err", err)
		os.Exit(1)
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
