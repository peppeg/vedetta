#!/usr/bin/env bash
# Separate Vedetta instance for replay and live benchmarks (V25-R2.2, V25-M1.1/M1.2).
#
# The production service (port 3030, cache in record mode) is never touched or
# restarted. The benchmark instance lives in its own working directory, so it has
# its own .env, config, database and cache:
#
#   ~/vedetta-bench/            (override with VEDETTA_BENCH_DIR)
#     .env                      generated from the repo .env (0600, never printed)
#     data/config.local         copied from the repo, with the profile fields on top (--profile)
#     data/vedetta.db           own sessions
#     data/cache-run/           fresh copy of the frozen corpus, rewritten by every prepare
#     data/cache-live-NAME/     live rounds: new empty cache in record mode, kept afterwards
#     profile.txt, mode.txt     what the last prepare set up
#     vedetta                   built from the repo at prepare time, here and never in bin/ (bin/vedetta is
#                               the production binary); or a copy of VEDETTA_BENCH_BINARY, an older build,
#                               with VEDETTA_BENCH_COMMIT naming its commit in commit.txt
#     commit.txt                the commit built, "-dirty" when Go files, web/, go.mod/sum or VERSION had changes
#     vedetta.log, vedetta.pid
#
# Usage:
#   scripts/bench_instance.sh prepare [--profile NAME] [--live ROUND] [CORPUS]
#       default: home config, replay of CORPUS (default: newest data/cache-corpus-*)
#       --profile NAME  overlay scripts/profiles/NAME.json on the home config.local
#                       (its "_env" object sets environment switches of the instance, e.g. VEDETTA_DEEP_LOOP_QUOTES);
#                       ${VAR} placeholders are filled from the repo .env (never printed)
#       --live ROUND    no corpus: record into a new empty data/cache-live-ROUND
#   scripts/bench_instance.sh start
#   scripts/bench_instance.sh stop
#   scripts/bench_instance.sh status
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BENCH="${VEDETTA_BENCH_DIR:-$HOME/vedetta-bench}"
PORT="${VEDETTA_BENCH_PORT:-3031}"
BIN="${VEDETTA_BENCH_BINARY:-}"
PIDFILE="$BENCH/vedetta.pid"
LOG="$BENCH/vedetta.log"

die() { echo "bench_instance: $*" >&2; exit 1; }

running_pid() {
	[[ -f "$PIDFILE" ]] || return 1
	local pid
	pid="$(cat "$PIDFILE")"
	[[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null && echo "$pid"
}

port_busy() {
	ss -Htln "sport = :$PORT" | grep -q .
}

# Writes the home config.local with the profile fields on top to $2. Placeholders
# ${VAR} take their value from the repo .env, ${home:FIELD} from the home
# config.local (V25-T7.1.5); a missing or empty one is an error.
write_profile_config() {
	local profile="$1" dest="$2"
	python3 - "$REPO/data/config.local" "$profile" "$REPO/.env" "$dest" <<'PY'
import json, re, sys
home, profile, envfile, dest = sys.argv[1:]
env = {}
for line in open(envfile):
    m = re.match(r"\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*?)\s*$", line)
    if m and not line.lstrip().startswith("#"):
        env[m.group(1)] = m.group(2).strip('"').strip("'")
cfg = json.load(open(home))
# map R3: the levers chosen in the home Settings stay out; the bench sets them with
# the profile's "_env" (or as fields of the profile)
for k in ("deep_loop_review_lines", "deep_loop_push", "deep_loop_grant", "quiet_log"):
    cfg.pop(k, None)
def fill(v):
    if isinstance(v, str):
        def sub(m):
            src, name = (cfg, m.group(2)) if m.group(1) else (env, m.group(2))
            if not isinstance(src.get(name), str) or not src.get(name):
                where = "the home config.local" if m.group(1) else ".env"
                sys.exit(f"bench_instance: {name} is missing or empty in {where} (needed by {profile})")
            return src[name]
        return re.sub(r"\$\{(home:)?([A-Za-z_][A-Za-z0-9_]*)\}", sub, v)
    if isinstance(v, list):
        return [fill(x) for x in v]
    if isinstance(v, dict):
        return {k: fill(x) for k, x in v.items()}
    return v
for k, v in json.load(open(profile)).items():
    if not k.startswith("_"):
        cfg[k] = fill(v)
with open(dest, "w") as f:
    json.dump(cfg, f, indent=2)
PY
}

cmd_prepare() {
	local profile="" live=""
	while [[ $# -gt 0 ]]; do
		case "$1" in
			--profile) [[ -n "${2:-}" ]] || die "--profile needs a name"; profile="$2"; shift 2 ;;
			--live) [[ -n "${2:-}" ]] || die "--live needs a round name"; live="$2"; shift 2 ;;
			-*) die "unknown option: $1" ;;
			*) break ;;
		esac
	done
	local profile_file=""
	if [[ -n "$profile" ]]; then
		profile_file="$REPO/scripts/profiles/$profile.json"
		[[ -f "$profile_file" ]] || die "unknown profile: $profile (no $profile_file)"
	fi
	running_pid >/dev/null && die "instance is running (pid $(cat "$PIDFILE")); stop it first"
	if [[ -n "$BIN" ]]; then
		[[ -x "$BIN" ]] || die "missing $BIN (VEDETTA_BENCH_BINARY)"
	else
		command -v go >/dev/null || die "go is needed to build the instance"
	fi
	[[ -f "$REPO/.env" ]] || die "missing $REPO/.env"
	if [[ -n "$live" ]]; then
		[[ $# -eq 0 ]] || die "--live takes no CORPUS"
		[[ "$live" =~ ^[A-Za-z0-9._-]+$ ]] || die "bad round name: $live"
		[[ -e "$BENCH/data/cache-live-$live" ]] && die "$BENCH/data/cache-live-$live already exists; pick another round name"
		prepare_instance "$profile_file" record "$BENCH/data/cache-live-$live"
		mkdir -p "$BENCH/data/cache-live-$live"
		echo "live $live" > "$BENCH/mode.txt"
		rm -f "$BENCH/corpus.txt"
		echo "prepared $BENCH: profile ${profile:-home}, live round, empty cache $BENCH/data/cache-live-$live (record), repo HEAD $(cat "$BENCH/commit.txt" 2>/dev/null || echo '?')"
		return 0
	fi
	prepare_replay "$profile_file" "${1:-}"
	echo "replay" > "$BENCH/mode.txt"
	echo "prepared $BENCH: profile ${profile:-home}, $(prepared_corpus_line)"
}

prepare_instance() {
	local profile_file="$1" mode="$2" cache_dir="$3"
	umask 077
	mkdir -p "$BENCH/data"
	if [[ -n "$profile_file" ]]; then
		write_profile_config "$profile_file" "$BENCH/data/config.local"
		basename "$profile_file" .json > "$BENCH/profile.txt"
	else
		# map R3: without the levers chosen in the home Settings (the bench sets them in .env)
		python3 -c 'import json, sys
cfg = json.load(open(sys.argv[1]))
for k in ("deep_loop_review_lines", "deep_loop_push", "deep_loop_grant", "quiet_log"):
    cfg.pop(k, None)
json.dump(cfg, open(sys.argv[2], "w"), indent=2)' "$REPO/data/config.local" "$BENCH/data/config.local"
		echo home > "$BENCH/profile.txt"
	fi
	# V25-QD1.8: a profile may set environment switches with "_env": {"NAME": "value"}; they replace the repo ones.
	# VEDETTA_CACHE_READ_DATE (Piano dopo audit 3.6) is not copied from the repo .env: in a replay
	# round the pages of the corpus are old and the model would see a date line on each. A profile
	# may still set it.
	local profile_env="" env_keys="VEDETTA_PORT|VEDETTA_CACHE_MODE|VEDETTA_CACHE_DIR|VEDETTA_CONFIG_PATH|VEDETTA_CACHE_READ_DATE|VEDETTA_CACHE_PRUNE|VEDETTA_BIND|VEDETTA_ACCESS_CODE|VEDETTA_QUIET_LOG"
	if [[ -n "$profile_file" ]]; then
		profile_env="$(python3 -c 'import json,sys
for k, v in (json.load(open(sys.argv[1])).get("_env") or {}).items():
    assert k.replace("_", "").isalnum(), k
    print(f"{k}={v}")' "$profile_file")" || die "bad _env in $profile_file"
		local k
		for k in $(printf '%s\n' "$profile_env" | cut -d= -f1); do env_keys="$env_keys|$k"; done
	fi
	{
		grep -v -E "^[[:space:]]*($env_keys)[[:space:]]*=" "$REPO/.env" || true
		echo
		echo "# V25-R2/M1: benchmark instance (generated by scripts/bench_instance.sh)"
		echo "VEDETTA_PORT=$PORT"
		# map R6: the bench answers only on this machine and asks no code
		echo "VEDETTA_BIND=127.0.0.1"
		echo "VEDETTA_CACHE_MODE=$mode"
		echo "VEDETTA_CACHE_DIR=$cache_dir"
		# on by default since map R2: off here unless the profile sets it (see above)
		printf '%s\n' "$profile_env" | grep -q '^VEDETTA_CACHE_READ_DATE=' || echo "VEDETTA_CACHE_READ_DATE=false"
		# map R2: no deleting of expired entries, a live round's folder is replayed later whole
		printf '%s\n' "$profile_env" | grep -q '^VEDETTA_CACHE_PRUNE=' || echo "VEDETTA_CACHE_PRUNE=false"
		[[ -n "$profile_env" ]] && printf '%s\n' "$profile_env"
	} > "$BENCH/.env"
	chmod 600 "$BENCH/.env" "$BENCH/data/config.local"
	snapshot_binary
}

# snapshot_binary puts the instance's program in $BENCH: built from the repo, or a
# copy of VEDETTA_BENCH_BINARY. Never "make build": it writes bin/vedetta, which
# the production service runs (Piano dopo audit 2.1, rule 15).
snapshot_binary() {
	if [[ -n "$BIN" ]]; then
		cp "$BIN" "$BENCH/vedetta.new"
	else
		(cd "$REPO" && go build -ldflags "-X main.version=$(cat VERSION)" -o "$BENCH/vedetta.new" main.go) || die "build failed"
	fi
	mv -f "$BENCH/vedetta.new" "$BENCH/vedetta"
	if [[ -n "${VEDETTA_BENCH_COMMIT:-}" ]]; then
		echo "$VEDETTA_BENCH_COMMIT" > "$BENCH/commit.txt"
	else
		local commit
		commit="$(git -C "$REPO" rev-parse --short HEAD 2>/dev/null || echo unknown)"
		if [[ -z "$BIN" && -n "$(git -C "$REPO" status --porcelain -- '*.go' go.mod go.sum VERSION web 2>/dev/null)" ]]; then
			commit="$commit-dirty"
		fi
		echo "$commit" > "$BENCH/commit.txt"
	fi
}

prepare_replay() {
	local profile_file="$1" corpus="$2"
	if [[ -z "$corpus" ]]; then
		corpus="$(ls -d "$REPO"/data/cache-corpus-* 2>/dev/null | sort | tail -1 || true)"
		[[ -n "$corpus" ]] || die "no data/cache-corpus-* found; pass CORPUS explicitly"
	fi
	local given="$corpus"
	corpus="$(cd "$corpus" && pwd)" || die "corpus not found: $given"
	[[ -d "$corpus/search" || -d "$corpus/page" ]] || die "$corpus does not look like a cache (no search/ or page/)"

	prepare_instance "$profile_file" replay "$BENCH/data/cache-run"

	# Fresh copy every time: replay saves misses, so a reused copy would favor later rounds.
	rm -rf "$BENCH/data/cache-run"
	cp -r "$corpus" "$BENCH/data/cache-run"
	chmod -R u+w "$BENCH/data/cache-run"

	echo "$corpus" > "$BENCH/corpus.txt"
}

prepared_corpus_line() {
	local n_search n_page
	n_search="$(find "$BENCH/data/cache-run/search" -type f 2>/dev/null | wc -l)"
	n_page="$(find "$BENCH/data/cache-run/page" -type f 2>/dev/null | wc -l)"
	echo "corpus=$(cat "$BENCH/corpus.txt") (search files $n_search, page files $n_page), repo HEAD $(cat "$BENCH/commit.txt" 2>/dev/null || echo '?')"
}

cmd_start() {
	[[ -f "$BENCH/.env" && -x "$BENCH/vedetta" ]] || die "not prepared; run: $0 prepare"
	local pid
	if pid="$(running_pid)"; then die "already running (pid $pid)"; fi
	port_busy && die "port $PORT is already in use"

	[[ -f "$LOG" ]] && mv -f "$LOG" "$LOG.prev"
	# Clean environment: Vedetta lets existing variables win over .env.
	(cd "$BENCH" && exec setsid env -i HOME="$HOME" PATH="$PATH" ./vedetta >"$LOG" 2>&1 </dev/null) &
	echo $! > "$PIDFILE"

	for _ in $(seq 1 30); do
		if curl -fs -o /dev/null --max-time 2 "http://localhost:$PORT/"; then
			echo "started pid $(cat "$PIDFILE") on port $PORT"
			cmd_status
			return 0
		fi
		running_pid >/dev/null || { tail -20 "$LOG" >&2; die "instance exited during startup (log: $LOG)"; }
		sleep 1
	done
	cmd_stop >/dev/null || true
	tail -20 "$LOG" >&2
	die "no answer on port $PORT after 30 s; stopped (log: $LOG)"
}

cmd_stop() {
	local pid
	if ! pid="$(running_pid)"; then
		rm -f "$PIDFILE"
		echo "not running"
		return 0
	fi
	kill "$pid"
	for _ in $(seq 1 15); do
		kill -0 "$pid" 2>/dev/null || { rm -f "$PIDFILE"; echo "stopped pid $pid"; return 0; }
		sleep 1
	done
	kill -9 "$pid" 2>/dev/null || true
	rm -f "$PIDFILE"
	echo "killed pid $pid (did not stop within 15 s)"
}

cmd_status() {
	local pid cache
	if pid="$(running_pid)"; then echo "running: pid $pid, port $PORT"; else echo "not running (port $PORT)"; fi
	cache="$(grep -o 'Search and page cache: .*' "$LOG" 2>/dev/null | tail -1 || true)"
	echo "cache: ${cache#Search and page cache: }"
	[[ -f "$BENCH/profile.txt" ]] && echo "profile: $(cat "$BENCH/profile.txt")"
	[[ -f "$BENCH/mode.txt" ]] && echo "mode: $(cat "$BENCH/mode.txt")"
	[[ -f "$BENCH/corpus.txt" ]] && echo "corpus: $(cat "$BENCH/corpus.txt")"
	[[ -f "$BENCH/commit.txt" ]] && echo "repo HEAD at prepare: $(cat "$BENCH/commit.txt")"
	return 0
}

case "${1:-}" in
	prepare) shift; cmd_prepare "$@" ;;
	start) cmd_start ;;
	stop) cmd_stop ;;
	status) cmd_status ;;
	*) echo "usage: $0 prepare [--profile NAME] [--live ROUND] [CORPUS] | start | stop | status" >&2; exit 2 ;;
esac
