#!/usr/bin/env bash
#
# test.sh - manual test units for hetty graceful shutdown changes.
#
# Usage:
#   ./test.sh all             Run all test units (default)
#   ./test.sh build           Build + vet + cross-compile checks
#   ./test.sh unit            Run Go unit tests
#   ./test.sh shutdown        E2E: graceful shutdown closes tunnels/idle conns
#   ./test.sh db              E2E: database survives shutdown, can be reopened
#   ./test.sh portconflict    E2E: startup failure path exits non-zero
#
set -euo pipefail

cd "$(dirname "$0")"

PORT="${HETTY_TEST_PORT:-18080}"
UPSTREAM_PORT="${HETTY_TEST_UPSTREAM_PORT:-18081}"
STARTUP_TIMEOUT=15
SHUTDOWN_TIMEOUT=15

WORK=""
HETTY_PID=""
UPSTREAM_PID=""
EMBED_PLACEHOLDER="cmd/hetty/admin/_next/static/test_embed_placeholder.js"

log()  { printf '\033[1;34m[test]\033[0m %s\n' "$*"; }
pass() { printf '\033[1;32m[pass]\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m[fail]\033[0m %s\n' "$*" >&2; exit 1; }

cleanup() {
	[ -n "$HETTY_PID" ] && kill -INT "$HETTY_PID" 2>/dev/null || true
	[ -n "$UPSTREAM_PID" ] && kill "$UPSTREAM_PID" 2>/dev/null || true
	[ -n "$WORK" ] && rm -rf "$WORK"
	rm -f "$EMBED_PLACEHOLDER"
}
trap cleanup EXIT

setup_workdir() {
	WORK="$(mktemp -d)"
	mkdir -p "$WORK/www"
	echo "hetty-test-upstream" > "$WORK/www/index.html"
	# The admin frontend is stubbed in this repo; `go:embed` needs at least
	# one embeddable file in admin/_next/static. Removed by the EXIT trap.
	[ -e "$EMBED_PLACEHOLDER" ] || echo "// test placeholder" > "$EMBED_PLACEHOLDER"
}

build_binary() {
	log "building hetty binary ..."
	go build -o "$WORK/hetty" ./cmd/hetty
}

start_hetty() {
	local db="$1"
	"$WORK/hetty" --addr "127.0.0.1:$PORT" \
		--db "$db" \
		--cert "$WORK/hetty_cert.pem" \
		--key "$WORK/hetty_key.pem" \
		--json >"$WORK/hetty.log" 2>&1 &
	HETTY_PID=$!
}

wait_healthy() {
	local _i
	for _i in $(seq 1 $((STARTUP_TIMEOUT * 10))); do
		if grep -q "Startup health check passed" "$WORK/hetty.log" 2>/dev/null; then
			return 0
		fi
		if ! kill -0 "$HETTY_PID" 2>/dev/null; then
			cat "$WORK/hetty.log" >&2
			fail "hetty exited before becoming healthy"
		fi
		sleep 0.1
	done
	cat "$WORK/hetty.log" >&2
	fail "hetty did not pass startup health check within ${STARTUP_TIMEOUT}s"
}

stop_hetty() {
	kill -INT "$HETTY_PID"
	local _i
	for _i in $(seq 1 $((SHUTDOWN_TIMEOUT * 10))); do
		if ! kill -0 "$HETTY_PID" 2>/dev/null; then
			break
		fi
		sleep 0.1
	done
	if kill -0 "$HETTY_PID" 2>/dev/null; then
		kill -KILL "$HETTY_PID" 2>/dev/null || true
		fail "hetty did not exit within ${SHUTDOWN_TIMEOUT}s after SIGINT"
	fi
	local code=0
	wait "$HETTY_PID" || code=$?
	HETTY_PID=""
	[ "$code" -eq 0 ] || fail "hetty exited with code $code, expected 0"
}

start_upstream() {
	python3 -m http.server "$UPSTREAM_PORT" --bind 127.0.0.1 \
		--directory "$WORK/www" >"$WORK/upstream.log" 2>&1 &
	UPSTREAM_PID=$!
	sleep 1
	kill -0 "$UPSTREAM_PID" 2>/dev/null || fail "upstream http.server failed to start"
}

# --- test units --------------------------------------------------------------

test_build() {
	log "unit: build"
	setup_workdir
	go build ./...
	go vet ./...
	GOOS=linux go build ./...
	GOOS=windows go build ./pkg/...
	pass "build (incl. vet and linux/windows cross-compile)"
}

test_unit() {
	log "unit: go test"
	go test ./...
	pass "go unit tests"
}

test_shutdown() {
	log "unit: graceful shutdown (tunnels + idle conns + SIGINT exit code)"
	setup_workdir
	build_binary
	start_upstream
	start_hetty "$WORK/hetty.db"
	wait_healthy
	log "hetty healthy (pid $HETTY_PID)"

	# 1. Plain HTTP request through the proxy.
	local code
	code="$(curl -sS -o /dev/null -w '%{http_code}' \
		-x "http://127.0.0.1:$PORT" "http://127.0.0.1:$UPSTREAM_PORT/")"
	[ "$code" = "200" ] || fail "proxied HTTP request returned $code, want 200"
	log "proxied HTTP request OK"

	# 2. Open a CONNECT tunnel and keep it open (TLS handshake left pending,
	#    simulating a client still waiting on the tunnel).
	exec 3<>"/dev/tcp/127.0.0.1/$PORT"
	printf 'CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n' >&3
	local resp
	IFS= read -r -t 5 resp <&3 || fail "no response to CONNECT"
	case "$resp" in
		*" 200"*) ;;
		*) fail "unexpected CONNECT response: $resp" ;;
	esac
	log "CONNECT tunnel established"

	# 3. SIGINT and expect a clean, timely exit.
	stop_hetty
	grep -q "Shutting down HTTP server" "$WORK/hetty.log" \
		|| fail "shutdown log line missing"
	! grep -q "HTTP server closed unexpected" "$WORK/hetty.log" \
		|| fail "unexpected server error in log"

	# 4. The tunnel connection must have been closed by the proxy (EOF).
	local eof=""
	IFS= read -r -t 5 eof <&3 || true
	exec 3<&-
	[ -z "$eof" ] || fail "tunnel still usable after shutdown"
	pass "graceful shutdown (exit 0, tunnel closed, logs clean)"
}

test_db() {
	log "unit: database closed cleanly and reopenable"
	setup_workdir
	build_binary
	start_hetty "$WORK/hetty.db"
	wait_healthy
	stop_hetty
	[ -f "$WORK/hetty.db" ] || fail "database file missing after shutdown"

	# Reopen the same database: must become healthy again (no corruption,
	# no stale lock; freelist is rebuilt after NoFreelistSync).
	: > "$WORK/hetty.log"
	start_hetty "$WORK/hetty.db"
	wait_healthy
	stop_hetty
	pass "database reopen after shutdown"
}

test_portconflict() {
	log "unit: startup failure path (port in use) exits non-zero"
	setup_workdir
	build_binary
	start_hetty "$WORK/hetty-a.db"
	wait_healthy

	# Second instance on the same address must fail promptly. Its deferred
	# DB close must run (no Fatal in Exec), so its own db stays consistent.
	"$WORK/hetty" --addr "127.0.0.1:$PORT" \
		--db "$WORK/hetty-b.db" \
		--cert "$WORK/hetty_cert.pem" \
		--key "$WORK/hetty_key.pem" \
		--json >"$WORK/hetty-b.log" 2>&1 &
	local pid_b=$!
	local code=0
	wait "$pid_b" || code=$?
	[ "$code" -ne 0 ] || fail "conflicting instance exited 0, want non-zero"
	grep -q "HTTP server closed unexpectedly" "$WORK/hetty-b.log" \
		|| fail "expected startup error in log of conflicting instance"
	log "conflicting instance exited with code $code as expected"

	# First instance must still be healthy and shut down cleanly.
	local acode
	acode="$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/")"
	[ "$acode" = "200" ] || fail "first instance not healthy after conflict"
	stop_hetty
	pass "startup failure path"
}

# --- main --------------------------------------------------------------------

units="${*:-all}"
for unit in $units; do
	case "$unit" in
		all)
			test_build
			test_unit
			test_shutdown
			test_db
			test_portconflict
			;;
		build)        test_build ;;
		unit)         test_unit ;;
		shutdown)     test_shutdown ;;
		db)           test_db ;;
		portconflict) test_portconflict ;;
		*) fail "unknown unit: $unit (want: all|build|unit|shutdown|db|portconflict)" ;;
	esac
done

log "all requested units passed"
