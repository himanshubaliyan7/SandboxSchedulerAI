package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"time"
)

// startEgressBridge listens on addr (TCP, loopback inside the sandbox)
// and pipes every connection to the Unix socket at socketPath, where the
// host's egress proxy listens. The sandbox has no route out, so this
// bridge is the only way a process in it reaches the proxy.
func startEgressBridge(ctx context.Context, addr, socketPath string, logger *log.Logger) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("egress bridge: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
					return
				}
				logger.Printf("egress bridge: accept: %v", err)
				time.Sleep(50 * time.Millisecond)
				continue
			}
			go bridge(ctx, c, socketPath, logger)
		}
	}()
	return ln, nil
}

func bridge(ctx context.Context, client net.Conn, socketPath string, logger *log.Logger) {
	defer client.Close()
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	up, err := d.DialContext(dctx, "unix", socketPath)
	cancel()
	if err != nil {
		logger.Printf("egress bridge: %v", err)
		return
	}
	defer up.Close()

	stop := context.AfterFunc(ctx, func() {
		_ = client.Close()
		_ = up.Close()
	})
	defer stop()

	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		// Propagate EOF without tearing down the other direction.
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(up, client)
	go pipe(client, up)
	<-done
	<-done
}

// proxyEnv is the proxy configuration exported to processes. addr is the
// bridge's listen address.
func proxyEnv(addr string) []string {
	url := "http://" + addr
	noProxy := "localhost,127.0.0.1,::1"
	return []string{
		"HTTP_PROXY=" + url, "http_proxy=" + url,
		"HTTPS_PROXY=" + url, "https_proxy=" + url,
		"ALL_PROXY=" + url, "all_proxy=" + url,
		"NO_PROXY=" + noProxy, "no_proxy=" + noProxy,
	}
}
