#!/usr/bin/env bash
# Run from the repository root against a locally built amd64 preview image.
set -euo pipefail
image=${1:?image required}
commit=${2:?full commit required}
tmp=$(mktemp -d)
name="uptime-preview-${tmp##*/}"
volume="$name-data"
cleanup() {
  result=$?
  if [ "$result" -ne 0 ]; then docker logs "$name" >&2 || true; fi
  for container in "$name" "$name-check"; do
    if docker container inspect "$container" >/dev/null 2>&1; then
      docker rm -f "$container" >/dev/null || result=1
    fi
  done
  if docker volume inspect "$volume" >/dev/null 2>&1; then
    docker volume rm "$volume" >/dev/null || result=1
  fi
  rm -rf -- "$tmp" || result=1
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

test "$(docker image inspect --format '{{.Os}}/{{.Architecture}} {{.Config.User}}' "$image")" = 'linux/amd64 65532:65532'
version=$(docker run --rm --name "$name-check" "$image" version)
printf '%s\n' "$version"
grep -Fx 'LibreCosta Uptime dev' <<< "$version"
grep -Fx "Commit: $commit" <<< "$version"
docker run --rm --name "$name-check" "$image" --help
docker run --rm --name "$name-check" \
  --mount "type=bind,src=$PWD/deploy/docker/uptime.example.yaml,dst=/etc/uptime/uptime.yaml,readonly" \
  "$image" config check --config /etc/uptime/uptime.yaml

# Inspect without a shell or additional programs in the runtime image.
docker create --name "$name-check" "$image" >/dev/null
docker cp "$name-check:/etc/ssl/certs/ca-certificates.crt" "$tmp/ca.crt"
grep -q 'BEGIN CERTIFICATE' "$tmp/ca.crt"
docker cp "$name-check:/usr/share/licenses/librecosta-uptime/LICENSE" "$tmp/LICENSE"
cmp LICENSE "$tmp/LICENSE"
docker export --output "$tmp/rootfs.tar" "$name-check"
tar -tf "$tmp/rootfs.tar" > "$tmp/files"
if grep -Eq '^(src/|usr/local/go/|bin/sh$|etc/uptime/uptime.yaml$|data/uptime.db$|.*\.env$|.*\.key$)' "$tmp/files"; then
  echo 'Unexpected source/toolchain/config/data/private key in runtime image' >&2
  exit 1
fi
docker rm "$name-check" >/dev/null
if docker run --rm --name "$name-check" "$image"; then
  echo 'Missing configuration unexpectedly succeeded' >&2
  exit 1
else
  test "$?" -eq 1
fi

docker volume create "$volume" >/dev/null
write_config() {
  cat > "$tmp/uptime.yaml" <<EOF
server:
  address: ":18580"
storage:
  type: bbolt
  bbolt:
    path: /data/uptime.db
uptime:
  interval: 1s
endpoints:
  - id: $1
    url: http://127.0.0.1:1/
EOF
  chmod 644 "$tmp/uptime.yaml"
}
start_and_check() {
  docker run -d --name "$name" --stop-timeout 20 --read-only \
    --cap-drop ALL --security-opt no-new-privileges \
    -p 127.0.0.1::18580 \
    --mount "type=volume,src=$volume,dst=/data" \
    --mount "type=bind,src=$tmp/uptime.yaml,dst=/etc/uptime/uptime.yaml,readonly" \
    "$image" >/dev/null
  address=$(docker port "$name" 18580/tcp)
  ready=false
  for attempt in {1..30}; do
    if [ "$(curl -fsS --max-time 2 "http://$address/readyz" 2>/dev/null)" = ready ]; then
      ready=true
      break
    fi
    sleep 1
  done
  test "$ready" = true
  test "$(curl -fsS --max-time 2 "http://$address/livez")" = ok
  test "$(curl -sS --max-time 2 -D "$tmp/headers" -o /dev/null -w '%{http_code}' "http://$address/")" = 302
  tr -d '\r' < "$tmp/headers" | grep -Eiq '^Location: /uptime$'
  docker stop "$name" >/dev/null
  test "$(docker inspect --format '{{.State.ExitCode}}' "$name")" = 0
}
write_config retired
start_and_check
docker cp "$name:/data/uptime.db" "$tmp/uptime.db"
test -s "$tmp/uptime.db"
docker rm "$name" >/dev/null

# A different YAML ID prevents a fresh database from accidentally passing this test.
write_config replacement
start_and_check
docker rm "$name" >/dev/null
services=$(docker run --rm --name "$name-check" \
  --mount "type=volume,src=$volume,dst=/data" \
  --mount "type=bind,src=$tmp/uptime.yaml,dst=/etc/uptime/uptime.yaml,readonly" \
  "$image" service list --config /etc/uptime/uptime.yaml)
printf '%s\n' "$services"
grep -Eq '^retired[[:space:]]+detached' <<< "$services"
grep -Eq '^replacement[[:space:]]+active' <<< "$services"
if docker run --rm --name "$name-check" \
  --mount "type=volume,src=$volume,dst=/data,readonly" \
  --mount "type=bind,src=$tmp/uptime.yaml,dst=/etc/uptime/uptime.yaml,readonly" "$image"; then
  echo 'Read-only bbolt storage unexpectedly succeeded' >&2
  exit 1
else
  test "$?" -eq 1
fi
echo 'amd64 deployment, SIGTERM, permissions and persistence checks passed'
