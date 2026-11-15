package proxy

// Streamable HTTP mode: the agent talks HTTP to us, we forward every request to the
// real MCP server (--upstream) and stream the answer back unchanged.
//
//   agent --POST /mcp--> nhi-probe proxy --POST--> real MCP server
//         <--JSON or SSE stream--         <--
//
// Sessions: MCP puts the session id in the "Mcp-Session-Id" header. Each session gets a
// kestrelShard. Sessions are spread over 16 buckets, each with its OWN lock, so two
// requests from different sessions almost never wait for each other (no global lock).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/krishitag/nhi-probe/internal/detect"
	"github.com/krishitag/nhi-probe/internal/event"
)

const (
	numBuckets = 16       // session buckets; each has its own mutex
	maxBody    = 10 << 20 // 10 MB: bigger requests are refused (memory safety)
)

type bucket struct {
	mu       sync.Mutex
	sessions map[string]*kestrelShard
}

// sessionTable maps session id -> kestrelShard, sharded by fnv(id) % 16.
type sessionTable struct {
	buckets [numBuckets]bucket
}

func newSessionTable() *sessionTable {
	t := &sessionTable{}
	for i := range t.buckets {
		t.buckets[i].sessions = map[string]*kestrelShard{}
	}
	return t
}

func (t *sessionTable) bucketFor(id string) *bucket {
	h := fnv.New32a()
	h.Write([]byte(id))
	return &t.buckets[h.Sum32()%numBuckets]
}

// getOrCreate returns the session's shard, making it if it's new.
func (t *sessionTable) getOrCreate(id string, create func() *kestrelShard) *kestrelShard {
	b := t.bucketFor(id)
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[id]
	if !ok {
		s = create()
		b.sessions[id] = s
	}
	return s
}

func (t *sessionTable) put(id string, s *kestrelShard) {
	b := t.bucketFor(id)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sessions[id] = s
}

func (t *sessionTable) remove(id string) {
	b := t.bucketFor(id)
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sessions, id)
}

// headerCreds finds credentials in HTTP headers: by signature, or a secret-looking
// header name (Authorization, X-Api-Key...). "Bearer " is removed first so the
// fingerprint matches the same token seen elsewhere (scan, env).
func headerCreds(h http.Header) []event.Cred {
	var out []event.Cred
	for name, values := range h {
		for _, v := range values {
			v = strings.TrimPrefix(v, "Bearer ")
			found := detect.Scan(v)
			if len(found) == 0 && (detect.IsSecretName(name) || strings.EqualFold(name, "Authorization")) && len(v) >= 8 {
				found = []detect.Finding{{Provider: "unknown", Secret: v}}
			}
			for _, f := range found {
				out = append(out, toCred(f, "header:"+name))
			}
		}
	}
	return out
}

// splitBatch: a JSON-RPC body can be one message or an array (batch) of messages.
func splitBatch(body []byte) [][]byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return [][]byte{body}
	}
	var msgs []json.RawMessage
	if json.Unmarshal(trimmed, &msgs) != nil {
		return [][]byte{body}
	}
	out := make([][]byte, len(msgs))
	for i, m := range msgs {
		out[i] = m
	}
	return out
}

// hop-by-hop headers belong to one connection only and must not be forwarded.
// Accept-Encoding is dropped so Go's client handles gzip itself and we can read the body.
var dropHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
	"Content-Length": true, "Accept-Encoding": true, "Content-Encoding": true,
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if dropHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

type httpProxy struct {
	ctx      context.Context // cancelled on Ctrl-C
	upstream string
	label    string
	client   *http.Client
	sessions *sessionTable
	events   *sink
	raw      *rawLog
}

func newHTTPProxy(ctx context.Context, upstream, label string, events *sink, raw *rawLog) (*httpProxy, error) {
	u, err := url.Parse(upstream)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("--upstream %q must be an http(s) URL", upstream)
	}
	return &httpProxy{
		ctx: ctx, upstream: upstream, label: label, events: events, raw: raw,
		client:   &http.Client{}, // no overall timeout: SSE streams can stay open a long time
		sessions: newSessionTable(),
	}, nil
}

func (p *httpProxy) newShard(id string) *kestrelShard {
	return &kestrelShard{session: id, label: p.label}
}

// ServeHTTP runs once per request, each in its own goroutine (net/http does that).
func (p *httpProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		http.Error(w, "read request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > maxBody {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}

	sid := r.Header.Get("Mcp-Session-Id")
	var shard *kestrelShard
	if sid != "" {
		shard = p.sessions.getOrCreate(sid, func() *kestrelShard { return p.newShard(sid) })
	} else {
		// First request (initialize / discover): no session yet. Use a private shard;
		// it gets published under the id the server returns.
		shard = p.newShard("pending-" + newSessionID())
	}

	if len(body) > 0 {
		p.raw.write("->", body)
		hdr := headerCreds(r.Header)
		for _, msg := range splitBatch(body) {
			if e, ok := shard.observeRequest(msg); ok {
				e.NHIs = mergeCreds(e.NHIs, hdr) // credentials on the call itself = direct use
				p.events.send(p.ctx, e)
			}
		}
	}

	out, err := http.NewRequestWithContext(r.Context(), r.Method, p.upstream, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "build upstream request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	copyHeaders(out.Header, r.Header)
	resp, err := p.client.Do(out)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if newID := resp.Header.Get("Mcp-Session-Id"); newID != "" && sid == "" {
		shard.session = newID // safe: nobody else has this shard until put() below
		p.sessions.put(newID, shard)
	}
	if r.Method == http.MethodDelete && sid != "" {
		p.sessions.remove(sid) // client ended the session: free its memory
	}

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	sse := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")

	// Stream the answer line by line: forward unchanged, flush SSE lines right away.
	reader := bufio.NewReader(resp.Body)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			p.raw.write("<-", line)
			if sse {
				if data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
					shard.observeReply(bytes.TrimSpace(data))
				}
			} else {
				shard.observeReply(line)
			}
			if _, err := w.Write(line); err != nil {
				return // agent went away
			}
			if sse && flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return // end of answer, or upstream/agent cancelled
		}
	}
}

// runHTTP serves until Ctrl-C, then shuts down within ~1 s and writes the last events.
func runHTTP(ctx context.Context, addr, upstream, label string, eventsFile io.Writer, raw *rawLog, bufSize int) error {
	events := newSink(eventsFile, bufSize)
	go events.run() // goroutine "event writer"
	defer events.stop()

	p, err := newHTTPProxy(ctx, upstream, label, events, raw)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: addr, Handler: p, ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }() // goroutine "http server" (+1 goroutine per request)
	fmt.Fprintf(os.Stderr, "nhi-probe: HTTP proxy on %s -> %s\n", addr, upstream)

	select {
	case err := <-errc:
		return fmt.Errorf("http server: %w", err) // e.g. port already in use
	case <-ctx.Done():
	}
	// Ctrl-C: stop accepting, give open requests 1 s, then close everything (SSE streams too).
	shCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		srv.Close()
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return ctx.Err()
}
