package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/voidgrid/voidgrid-soundscapes/internal/config"
)

// healthcheck probes the local /healthz and returns a process exit code. It lets the container check
// itself without shipping curl.
func healthcheck() int {
	// Only the listen address: this process does not get the server's secrets (see config.ListenAddr).
	listen, err := config.ListenAddr()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 2
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		return 2
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.Status)
		return 1
	}
	return 0
}
