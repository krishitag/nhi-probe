// loadgen sends many concurrent real tools/call requests to a Streamable HTTP MCP
// server and prints latency percentiles. Run it twice, against the server directly
// and through nhi-probe proxy: the difference is the latency the proxy adds.
//
//	go run ./cmd/loadgen --url http://127.0.0.1:3001/mcp --n 1000
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"
)

func main() {
	url := flag.String("url", "http://127.0.0.1:3001/mcp", "MCP endpoint")
	n := flag.Int("n", 1000, "number of tools/call requests, all started at once")
	flag.Parse()
	if err := run(*url, *n); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

func post(c *http.Client, url, session, body string) (*http.Response, error) {
	req, err := http.NewRequest("POST", url, bytes.NewBufferString(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	return c.Do(req)
}

func run(url string, n int) error {
	c := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: n, MaxConnsPerHost: 0}, Timeout: 60 * time.Second}

	// 1. Handshake (2025 protocol: initialize -> session id -> initialized).
	resp, err := post(c, url, "", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"nhi-loadgen","version":"1"}}}`)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	session := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != 200 {
		return fmt.Errorf("initialize: HTTP %d", resp.StatusCode)
	}
	resp, err = post(c, url, session, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// 2. n concurrent tools/call (echo), all released at the same moment.
	lat := make([]time.Duration, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"echo","arguments":{"message":"load %d"}}}`, i+1, i)
			t0 := time.Now()
			resp, err := post(c, url, session, body)
			if err != nil {
				errs[i] = err
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lat[i] = time.Since(t0)
			if resp.StatusCode != 200 || !bytes.Contains(b, []byte(`"result"`)) {
				errs[i] = fmt.Errorf("HTTP %d", resp.StatusCode)
			}
		}()
	}
	t0 := time.Now()
	close(start)
	wg.Wait()
	wall := time.Since(t0)

	var ok []time.Duration
	failed := 0
	for i := range n {
		if errs[i] != nil {
			failed++
		} else {
			ok = append(ok, lat[i])
		}
	}
	if len(ok) == 0 {
		return fmt.Errorf("all %d requests failed, e.g. %v", n, errs[0])
	}
	slices.Sort(ok)
	p := func(q float64) time.Duration { return ok[int(q*float64(len(ok)-1))] }
	fmt.Printf("url=%s n=%d ok=%d failed=%d wall=%v  p50=%v p90=%v p99=%v max=%v\n",
		url, n, len(ok), failed, wall.Round(time.Millisecond), p(0.50).Round(time.Microsecond),
		p(0.90).Round(time.Microsecond), p(0.99).Round(time.Microsecond), ok[len(ok)-1].Round(time.Microsecond))
	return nil
}
