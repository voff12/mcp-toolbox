#!/bin/bash
# Tries to get around Toolbox from inside an agent container, then reports each
# attempt. Exits non-zero if any control does not hold.
set -uo pipefail
cd "$(dirname "$0")"
REPO=$(cd ../.. && pwd)
COMPOSE="docker compose"
PASSED=0 FAILED=0

pass() { echo "PASS  $1"; PASSED=$((PASSED + 1)); }
fail() { echo "FAIL  $1"; FAILED=$((FAILED + 1)); }
cleanup() {
  $COMPOSE down -v --remove-orphans >/dev/null 2>&1
  rm -rf .run
}
trap cleanup EXIT

in_agent() { $COMPOSE exec -T agent bash -c "$1"; }
invoke() {
  in_agent "curl -s --max-time 10 -X POST http://toolbox:5000/api/tool/$1/invoke -H 'Content-Type: application/json' -d '$2'"
}
# True if a TCP connection opens. Used only to prove that one does not.
tcp_open() { in_agent "timeout 4 bash -c '</dev/tcp/$1/$2'" >/dev/null 2>&1; }

mkdir -p .run
head -c 18 /dev/urandom | base64 | tr -d '/+=' > .run/db_password
head -c 18 /dev/urandom | base64 | tr -d '/+=' > .run/mysql_root_password
DB_PASSWORD=$(cat .run/db_password)

echo "Building Toolbox..."
docker run --rm -v "$REPO":/src -w /src -v toolbox-boundary-gomod:/go/pkg/mod \
  -v toolbox-boundary-gobuild:/root/.cache/go-build golang:1.27.3 \
  go build -o /src/deploy/mysql-boundary/.run/toolbox . || exit 1
docker build -q -t toolbox-mysql-boundary:local -f - .run >/dev/null <<'DOCKERFILE' || exit 1
FROM golang:1.27.3
COPY toolbox /usr/local/bin/toolbox
DOCKERFILE

echo "Starting the stack..."
$COMPOSE up -d --wait mysql agent >/dev/null || exit 1
$COMPOSE up -d toolbox >/dev/null || exit 1
for _ in $(seq 1 30); do
  in_agent "curl -s --max-time 2 http://toolbox:5000" >/dev/null 2>&1 && break
  sleep 1
done

echo
echo "The controlled path works"
result=$(invoke query_customers '{"level":"gold"}')
if [[ $result == *'138****5678'* ]]; then pass "agent gets masked rows through Toolbox"; else fail "agent got: $result"; fi
if [[ $result == *13812345678* ]]; then fail "raw phone number in result"; else pass "raw phone number absent from result"; fi
result=$(invoke explain_query_customers '{"level":"gold"}')
if [[ $result == *accessType* ]]; then pass "agent gets a plan summary"; else fail "agent got: $result"; fi
result=$(invoke recommend_indexes '{"level":"gold"}')
if [[ $result == *requires_dba_approval* || $result == *notes* ]]; then pass "index advice runs as the read-only account"; else fail "got: $result"; fi

echo
echo "Network"
# A control: without it, a broken probe would look like a blocked port.
if tcp_open toolbox 5000; then pass "probe works: agent reaches Toolbox's port"; else fail "probe cannot reach Toolbox, so the checks below prove nothing"; fi
if in_agent "getent hosts mysql" >/dev/null 2>&1; then fail "agent can resolve the mysql name"; else pass "agent cannot resolve the mysql name"; fi
if tcp_open 172.30.1.20 3306; then fail "agent reached MySQL by address"; else pass "agent cannot reach MySQL by address"; fi
if tcp_open 172.30.1.10 5000; then fail "agent reached Toolbox's database-side address"; else pass "agent cannot reach the database network at all"; fi
if tcp_open 1.1.1.1 443; then fail "agent has outbound internet"; else pass "agent has no outbound internet"; fi

echo
echo "Credentials"
if in_agent "env; ls /run/secrets 2>/dev/null" | grep -q -e "$DB_PASSWORD" -e db_password; then
  fail "credentials visible in the agent environment"
else
  pass "no credentials in agent environment or secrets"
fi
if in_agent "grep -rIl -- '$DB_PASSWORD' /etc /home /root /tmp /run /go/src 2>/dev/null | grep -q ."; then
  fail "password found in agent files"
else
  pass "password not in agent files"
fi
if docker inspect "$($COMPOSE ps -q toolbox)" | grep -q -- "$DB_PASSWORD"; then
  fail "password visible in docker inspect"
else
  pass "password not in Toolbox container metadata"
fi

echo
echo "Database identity"
intruder() {
  docker run --rm --network toolbox-mysql-boundary_db-net --ip 172.30.1.99 mysql:8.4 \
    mysql -h 172.30.1.20 -u "$1" -p"$DB_PASSWORD" --connect-timeout=5 -e "$2" 2>&1
}
output=$(intruder toolbox_ro "SELECT 1")
if [[ $output == *"Access denied"* ]]; then
  pass "stolen password rejected from any other address, even on the database network"
else
  fail "stolen password worked: $output"
fi

echo
echo "Toolbox down"
$COMPOSE stop toolbox >/dev/null 2>&1
if invoke query_customers '{"level":"gold"}' | grep -q 138; then
  fail "agent still got data"
else
  pass "agent fails when Toolbox is down"
fi
if tcp_open 172.30.1.20 3306; then fail "a path to MySQL opened"; else pass "no fallback path to MySQL"; fi

echo
echo "Database identity at startup"
# Runs on Toolbox's own address, which is free now that it is stopped.
refused() { # account, expected text
  local log
  log=$($COMPOSE run --rm --no-deps -e DB_USER="$1" -T toolbox 2>&1 | tail -3)
  if [[ $log == *"$2"* ]]; then pass "Toolbox refuses to start as $1"; else fail "started as $1: $log"; fi
}
refused toolbox_rw "is not read-only"
refused toolbox_any "from any host"

echo
echo "$PASSED passed, $FAILED failed"
[[ $FAILED -eq 0 ]]
