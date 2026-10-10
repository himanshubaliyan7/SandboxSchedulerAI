package agent

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/himanshubaliyan7/warmbox/internal/agentapi"
	"github.com/himanshubaliyan7/warmbox/internal/runtime"
)

const usage = `usage:
  sbx-agent serve [flags]   run the in-sandbox agent
  sbx-agent version         print the agent version
`

// Main is the sbx-agent command line. args excludes the program name.
// It returns the process exit code.
func Main(args []string, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stderr, "sbx-agent %s (api %s)\n", AgentVersion, agentapi.Version)
		return 0
	case "serve":
		return serve(args[1:], stderr)
	}
	fmt.Fprint(stderr, usage)
	return 2
}

func serve(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("sbx-agent serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg Config
	var mode string
	fs.StringVar(&cfg.SocketPath, "socket", runtime.RunDir+"/"+runtime.AgentSocket, "Unix socket to listen on")
	fs.StringVar(&mode, "socket-mode", "0600", "permission bits of the socket (octal)")
	fs.StringVar(&cfg.Workspace, "workspace", runtime.Workdir, "default working directory and base of relative file paths")
	fs.IntVar(&cfg.RingBytes, "ring-bytes", agentapi.DefaultRingBytes, "per-process replay buffer in bytes")
	fs.DurationVar(&cfg.KeepaliveInterval, "keepalive", agentapi.KeepaliveInterval, "idle interval between keepalive events on a stream")
	fs.StringVar(&cfg.EgressSocket, "egress-socket", "", "Unix socket of the egress proxy (default: "+runtime.EgressSocket+" next to --socket)")
	fs.StringVar(&cfg.EgressListenAddr, "egress-listen", DefaultEgressListenAddr, "in-sandbox TCP address of the egress bridge")
	fs.DurationVar(&cfg.DrainGrace, "drain-grace", DefaultDrainGrace, "how long to keep reading output held open by descendants after a process exits")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "sbx-agent serve: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	m, err := strconv.ParseUint(mode, 8, 32)
	if err != nil || m > 0o777 {
		fmt.Fprintf(stderr, "sbx-agent serve: invalid --socket-mode %q\n", mode)
		return 2
	}
	cfg.SocketMode = os.FileMode(m)
	cfg.Logger = log.New(stderr, "sbx-agent: ", log.LstdFlags|log.LUTC)

	// As PID 1 the kernel re-parents every orphan in the sandbox to us.
	// When we are not PID 1 (tests, or a container started with an init),
	// ask for the same treatment so that process trees we start cannot
	// leave zombies under some other reaper. Failure is harmless.
	if os.Getpid() != 1 {
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
			cfg.Logger.Printf("PR_SET_CHILD_SUBREAPER: %v", err)
		}
	}
	cfg.ReapAll = true

	// PID 1 gets no default signal actions, so handle termination ourselves.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	a := New(cfg)
	cfg.Logger.Printf("version %s listening on %s (pid %d)", AgentVersion, cfg.SocketPath, os.Getpid())
	if err := a.Serve(ctx); err != nil {
		cfg.Logger.Printf("%v", err)
		return 1
	}
	return 0
}
