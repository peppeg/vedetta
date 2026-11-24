#!/usr/bin/env bash
# The installation as deploy/README.md describes it, on this machine: the image,
# essential, core, down to an answer with a source (the model is a fake one, so it
# costs nothing). GitHub runs it with Docker (.github/workflows/install.yml).
#
#   deploy/test/install.sh                                  Docker
#   ENGINE=podman COMPOSE=podman-compose deploy/test/install.sh
#
# SKIP_BUILD=1 uses the image already there. It works in a folder of its own and under
# a project name of its own, and removes what it started.
set -euo pipefail

ENGINE=${ENGINE:-docker}
COMPOSE=${COMPOSE:-docker compose}
PORT=${PORT:-3030}
MODEL_PORT=${MODEL_PORT:-11999}
SEARX_PORT=${SEARX_PORT:-8888}
repo=$(cd "$(dirname "$0")/../.." && pwd)
version=$(cat "$repo/VERSION")
image=ghcr.io/peppeg/vedetta:$version
searx_image=$(awk '/searxng\/searxng@sha256/ {print $2}' "$repo/deploy/essential/compose.yml")
work=$(mktemp -d)
base=http://127.0.0.1:$PORT
export COMPOSE_PROJECT_NAME=vedetta-install-test VEDETTA_HOST_PORT=$PORT
host=host.docker.internal
model_pid=

ok() { echo "  ok    $*"; }
fail() {
	echo "  FAIL  $*" >&2
	for s in essential core; do
		[[ -f $work/$s/.env ]] && (cd "$work/$s" && $COMPOSE logs --tail 15 2>&1 | cut -c1-200 | tail -50) >&2 || true
	done
	exit 1
}
cleanup() {
	[[ -n $model_pid ]] && kill "$model_pid" 2>/dev/null || true
	for s in essential core; do
		[[ -f $work/$s/.env ]] && (cd "$work/$s" && $COMPOSE down -v >/dev/null 2>&1) || true
	done
	$ENGINE rm -fv vedetta-install-searxng >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

container() { $ENGINE ps -aq --filter "label=com.docker.compose.project=$COMPOSE_PROJECT_NAME" --filter "label=com.docker.compose.service=$1"; }

# healthy waits, at most 5 minutes, for the named services to be healthy.
healthy() {
	local s i state
	for s in "$@"; do
		state=
		for ((i = 0; i < 60; i++)); do
			state=$($ENGINE inspect -f '{{.State.Health.Status}}' "$(container "$s")" 2>/dev/null || true)
			[[ $state == healthy ]] && break
			sleep 5
		done
		[[ $state == healthy ]] || fail "$s is not healthy ($state)"
	done
	ok "healthy: $*"
}

# say prints what a JSON expression of python gives for the answer d.
say() { python3 -c 'import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1]))' "$1"; }

volumes() { $ENGINE volume ls -q | sort; }

# ask puts the fake model in Settings, asks a question and checks the answer.
ask() {
	curl -fsS -o /dev/null -H 'Content-Type: application/json' "$base/api/config" \
		-d "{\"llm_base_url\":\"http://$host:$MODEL_PORT/v1\",\"llm_model\":\"fake\",\"setup_done\":true}" || fail "the model cannot be saved"
	curl -fsS -N --max-time 240 -H 'Content-Type: application/json' "$base/api/search" \
		-d '{"query":"Who may use the example domains?","answer_language":"en"}' >"$work/answer.txt" || fail "the question failed"
	grep -q "reserved for documentation" "$work/answer.txt" || { cut -c1-400 "$work/answer.txt" | tail -30; fail "no answer"; }
	grep -q "1 page read" "$work/answer.txt" || { cut -c1-400 "$work/answer.txt" | tail -30; fail "no page was read"; }
	ok "a question: an answer, one page read from the internet"
}

echo "== image"
if [[ -z ${SKIP_BUILD:-} ]]; then
	format=()
	[[ $ENGINE == podman ]] && format=(--format docker)
	$ENGINE build -q "${format[@]}" -t "$image" "$repo" >/dev/null
fi
user=$($ENGINE image inspect -f '{{.Config.User}}' "$image")
[[ -n $user && $user != 0 && $user != root ]] || fail "the image runs as root ($user)"
for s in essential core; do
	grep -q "image: $image\$" "$repo/deploy/$s/compose.yml" || fail "deploy/$s names another version than $version"
done
ok "built, user $user, the two packages name version $version"

python3 "$repo/deploy/test/fake_model.py" "$MODEL_PORT" &
model_pid=$!

echo "== essential"
cp -r "$repo/deploy/essential" "$repo/deploy/core" "$work/"
before=$(volumes)
cd "$work/essential"
cp .env.example .env
if $COMPOSE up -d >"$work/out.txt" 2>&1; then fail "it starts without SEARXNG_SECRET"; fi
grep -q "SEARXNG_SECRET" "$work/out.txt" || fail "the missing secret is not named"
ok "without the secret it stops and names it"
echo "SEARXNG_SECRET=$(openssl rand -hex 32)" >>.env
start=$(date +%s)
$COMPOSE up -d || fail "up -d"
healthy searxng jina vedetta
ok "up in $(($(date +%s) - start)) s, download included"
curl -fsS "$base/api/health" >/dev/null || fail "health"
curl -fsS "$base/" >"$work/page.html" || fail "the page does not answer"
grep -qi vedetta "$work/page.html" || fail "the page is not Vedetta's"
[[ $($ENGINE port "$(container vedetta)") == *"127.0.0.1:$PORT"* ]] || fail "the port is not on 127.0.0.1"
[[ -z $($ENGINE port "$(container searxng)")$($ENGINE port "$(container jina)") ]] || fail "SearXNG or Jina are published"
ok "page, health, published only on 127.0.0.1"
curl -fsS "$base/api/setup/discover" >"$work/found.json"
[[ $(say '[s["status"] for s in d["search"] if s["url"]=="http://searxng:8080"]' <"$work/found.json") == "['ok']" ]] || { cat "$work/found.json"; fail "SearXNG not found"; }
[[ $(say '[s["status"] for s in d["reader"] if s["url"]=="http://jina:8081"]' <"$work/found.json") == "['ok']" ]] || { cat "$work/found.json"; fail "Jina not found"; }
ok "the first run finds SearXNG ($(say '[(s.get("results"),s.get("problem","")) for s in d["search"]][0]' <"$work/found.json")) and Jina"
ask
$COMPOSE down >/dev/null 2>&1
$COMPOSE up -d >/dev/null 2>&1 || fail "up -d after down"
healthy vedetta
[[ $(curl -fsS "$base/api/sessions" | say 'len(d if isinstance(d, list) else d["sessions"])') == 1 ]] || fail "the search was lost after down and up"
ok "after down and up the search is still there"
if [[ $ENGINE == docker ]]; then
	$COMPOSE restart >/dev/null 2>&1 || fail "restart"
	healthy vedetta
fi
$COMPOSE down -v >/dev/null 2>&1
rm .env
[[ $(volumes) == "$before" ]] || { diff <(echo "$before") <(volumes) || true; fail "down -v leaves volumes"; }
ok "down -v leaves no volume"

echo "== core, with a SearXNG of one's own on this machine"
$ENGINE run -d --name vedetta-install-searxng -p "$SEARX_PORT:8080" -e SEARXNG_SECRET=only-for-the-test \
	-v "$repo/deploy/essential/searxng/settings.yml:/etc/searxng/settings.yml:ro,z" "$searx_image" >/dev/null
cd "$work/core"
cp .env.example .env
$COMPOSE up -d || fail "up -d"
healthy vedetta
found=
for ((i = 0; i < 12; i++)); do
	curl -fsS "$base/api/setup/discover?searxng=http://$host:$SEARX_PORT" >"$work/found.json"
	found=$(say "[s['status'] for s in d['search'] if s['url']=='http://$host:$SEARX_PORT']" <"$work/found.json")
	[[ $found == "['ok']" ]] && break
	sleep 5
done
[[ $found == "['ok']" ]] || { cat "$work/found.json"; fail "SearXNG on the host not found"; }
ok "SearXNG found at $host:$SEARX_PORT"
curl -fsS -o /dev/null -H 'Content-Type: application/json' "$base/api/config" -d "{\"searxng_url\":\"http://$host:$SEARX_PORT\"}" || fail "the address cannot be saved"
ask
echo "== all done"
