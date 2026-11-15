#!/bin/sh
# SCALING: 1,000 concurrent real tools/call requests to a real local MCP server
# (server-everything, Streamable HTTP), direct vs through nhi-probe proxy.
# Added latency = through-proxy minus direct, at p50 and p99.
#   sh lab/scale_proxy.sh
set -e
N=${1:-1000}
OUT=results/scaling
mkdir -p "$OUT"
ulimit -n 8192 # 1000+ sockets at once; macOS default is 256
go build -o nhi-probe ./cmd/nhi-probe

PORT=3001 npx -y @modelcontextprotocol/server-everything streamableHttp >/tmp/everything.log 2>&1 &
SERVER=$!
sleep 6

/usr/bin/time -l ./nhi-probe proxy --http 127.0.0.1:8788 --upstream http://127.0.0.1:3001/mcp \
  --name everything --events /tmp/load-events.jsonl 2>/tmp/proxy-time.txt &
PROXY=$!
sleep 1

{
  echo "== direct (no proxy), n=$N"
  go run ./cmd/loadgen --url http://127.0.0.1:3001/mcp --n "$N"
  echo "== through nhi-probe proxy, n=$N"
  go run ./cmd/loadgen --url http://127.0.0.1:8788/ --n "$N"
  echo "== second round (warm)"
  go run ./cmd/loadgen --url http://127.0.0.1:3001/mcp --n "$N"
  go run ./cmd/loadgen --url http://127.0.0.1:8788/ --n "$N"
} | tee "$OUT/proxy-$N.txt"

# Send Ctrl-C to the proxy itself (not to /usr/bin/time, which waits for it).
pkill -INT -f "nhi-probe proxy --http 127.0.0.1:8788" || true
wait $PROXY 2>/dev/null || true
kill $SERVER 2>/dev/null || true
{
  echo "== proxy process (Ctrl-C at the end)"
  grep -E "real|maximum resident" /tmp/proxy-time.txt
  echo "events recorded: $(wc -l < /tmp/load-events.jsonl)"
} | tee -a "$OUT/proxy-$N.txt"
rm -f /tmp/load-events.jsonl
