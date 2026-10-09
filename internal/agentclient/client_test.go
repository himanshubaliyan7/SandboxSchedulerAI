package agentclient_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agent"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentapi"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentclient"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/runtime"
)

// The method set the pool controller depends on.
var _ interface {
	WaitReady(ctx context.Context, hostRunDir string) error
	Init(ctx context.Context, hostRunDir string, req agentapi.InitRequest) error
} = agentclient.Sandboxes{}

func TestSandboxesWaitReadyAndInit(t *testing.T) {
	runDir, err := os.MkdirTemp("", "sbx")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(runDir)
	var sb agentclient.Sandboxes

	// No agent yet: WaitReady gives up with the context.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	err = sb.WaitReady(ctx, runDir)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitReady without an agent: %v, want deadline exceeded", err)
	}

	// The agent comes up while WaitReady is polling.
	a := agent.New(agent.Config{
		SocketPath: filepath.Join(runDir, runtime.AgentSocket),
		Workspace:  runDir,
	})
	actx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		served <- a.Serve(actx)
	}()
	defer func() {
		stop()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()

	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	begin := time.Now()
	if err := sb.WaitReady(ctx, runDir); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(begin); d < 150*time.Millisecond {
		t.Fatalf("WaitReady returned after %v, before the agent was started", d)
	}

	req := agentapi.InitRequest{SandboxID: "sb", Generation: 3, Env: map[string]string{"K": "v"}}
	if err := sb.Init(ctx, runDir, req); err != nil {
		t.Fatal(err)
	}
	req.Generation = 4
	if err := sb.Init(ctx, runDir, req); !agentclient.IsConflict(err) {
		t.Fatalf("Init with another generation: %v, want 409", err)
	}

	c := agentclient.ForRunDir(runDir)
	defer c.Close()
	if c.SocketPath() != agentclient.SocketPath(runDir) {
		t.Fatalf("socket path = %q", c.SocketPath())
	}
	h, err := c.Health(ctx)
	if err != nil || !h.Ready || !h.Initialized {
		t.Fatalf("health = %+v, %v", h, err)
	}
}

func TestErrorsWithoutAgent(t *testing.T) {
	c := agentclient.New("/nonexistent/sbx/agent.sock")
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.Health(ctx)
	if err == nil || agentclient.StatusOf(err) != 0 {
		t.Fatalf("Health without an agent: %v", err)
	}
	if _, err := c.DialWS(ctx, "x", 0); err == nil {
		t.Fatal("DialWS without an agent succeeded")
	}
	for _, err := range c.Stream(ctx, "x", 0) {
		if err == nil {
			t.Fatal("Stream without an agent yielded an event")
		}
	}
}
