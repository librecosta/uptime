# Docker deployment

Use Linux containers on an amd64 or arm64 Docker host. The versioned GHCR image
selects the matching architecture automatically.

## Get the image

```sh
docker pull ghcr.io/librecosta/uptime:v0.1.0
```

Use an explicit version tag for controlled upgrades. The image uses Distroless
static Debian 13 `nonroot`, with system CA certificates, UID/GID `65532:65532`, embedded Go timezone data,
and no shell or Go toolchain. Only the binary, MIT license and owned `/data`
directory are added. There is no automatic configuration generation or HEALTHCHECK.

## Configure and run

For a new installation on Linux, prepare an empty deployment directory and download
the [container example](../../deploy/docker/uptime.example.yaml):

```sh
curl --fail --location --output uptime.yaml \
  https://raw.githubusercontent.com/librecosta/uptime/v0.1.0/deploy/docker/uptime.example.yaml
```

Edit the endpoints before starting. Keep `server.address:
":18580"` and `storage.bbolt.path: /data/uptime.db`: container loopback cannot
receive published-port traffic, and the ordinary `./data/uptime.db` default is
relative to the process working directory (`/data` in this image).

The container must be able to read the mounted configuration. For a private file
on Linux, use `sudo chown root:65532 uptime.yaml` and `sudo chmod 0640 uptime.yaml`.
Do not mount TLS keys or configuration from a location the service cannot read.

```sh
IMAGE=ghcr.io/librecosta/uptime:v0.1.0
docker run --rm "$IMAGE" version
docker run --rm \
  --mount "type=bind,src=$(pwd)/uptime.yaml,dst=/etc/uptime/uptime.yaml,readonly" \
  "$IMAGE" config check --config /etc/uptime/uptime.yaml

docker volume create librecosta-uptime-data
docker run -d --name librecosta-uptime \
  --stop-timeout 20 --read-only --cap-drop ALL --security-opt no-new-privileges \
  -p 127.0.0.1:18580:18580 \
  --mount type=volume,src=librecosta-uptime-data,dst=/data \
  --mount "type=bind,src=$(pwd)/uptime.yaml,dst=/etc/uptime/uptime.yaml,readonly" \
  "$IMAGE"
```

A new named volume inherits the image directory's ownership; non-root can create
`/data/uptime.db`. Retain this named volume when replacing a container. Do not use
an anonymous volume or delete the named volume during upgrades. For a host bind
mount instead, an administrator must prepare its directory for UID/GID 65532,
for example `sudo install -d -m 0700 -o 65532 -g 65532 ./data`, and mount that
absolute path at `/data`. Existing data must also have compatible ownership.
The image does not run root startup scripts, change host ownership, or use mode 777.

PowerShell uses the same Docker flags, on one line or with backtick continuations.
Download the same example YAML and set `$IMAGE = 'ghcr.io/librecosta/uptime:v0.1.0'`.
Resolve the source first with `$configPath = (Resolve-Path ./uptime.yaml).Path`,
then pass `--mount "type=bind,src=$configPath,dst=/etc/uptime/uptime.yaml,readonly"`.
Docker Desktop must have access to that Windows path; Linux ownership commands
above apply to Linux hosts.

## Health and exposure

```sh
curl --fail http://127.0.0.1:18580/livez
curl --fail http://127.0.0.1:18580/readyz
curl -I http://127.0.0.1:18580/
docker logs librecosta-uptime
```

Open `http://127.0.0.1:18580/`; GET/HEAD redirects to `/uptime` unless `ui.path`
was changed. Liveness identifies a running process; readiness checks storage and
startup, not target availability. A monitored service being DOWN is not a reason
to restart the monitor. Choose external health checks for your actual port and
TLS hostname, trusting the appropriate CA; do not disable certificate verification.

The example has no authentication and publishes only on host loopback. For remote
access, explicitly configure a trusted TLS reverse proxy or
[native TLS with Basic Auth](../../README.md#native-tls-and-basic-auth),
appropriate firewall rules and port mapping. Plain HTTP Basic Auth does not
encrypt credentials. Mount native TLS certificates/keys read-only at configured
paths, readable by UID/GID 65532. Health routes stay public, including when Basic
Auth is enabled, but native TLS still applies. The image exposes metadata for port
18580; it does not override the application's loopback default or any supplied YAML.

## Stop, maintain, back up and upgrade

`docker stop librecosta-uptime` sends SIGTERM directly to Go. Keep Docker's stop
timeout greater than `server.shutdown_timeout` (20s versus the default 10s).
Increase both deliberately when needed; forced termination cannot guarantee safe
completion of blocked I/O.

Stop the serving container before bbolt backup or `service list/export/remove`.
For example, from the same deployment directory:

```sh
docker stop librecosta-uptime
umask 077
mkdir -p backups
docker cp librecosta-uptime:/data/uptime.db "backups/uptime-$(date -u +%Y%m%dT%H%M%SZ).db"
docker run --rm \
  --mount type=volume,src=librecosta-uptime-data,dst=/data \
  --mount "type=bind,src=$(pwd)/uptime.yaml,dst=/etc/uptime/uptime.yaml,readonly" \
  "$IMAGE" service list --config /etc/uptime/uptime.yaml
docker start librecosta-uptime
```

Verify each command succeeds before continuing; keep backups privately and avoid
reusing a backup filename. bbolt maintenance still needs a writable mount for its
exclusive file open. Export JSON is not a database backup and there is no import.
An export destination must be explicitly mounted writable or placed in `/data`
and copied out afterward. Removal retains the existing detached-only safeguards.
For Redis, reads may run online but are not a cross-call snapshot; coordinate all
writers before removing a service. Redis backup belongs to the Redis operator.

To upgrade, pull the chosen version from GHCR and verify its version with
`docker run --rm <image> version`. Stop, back up, then
`docker rm librecosta-uptime` and repeat the run command with the **same named
volume and configuration** and the new image. Recreate the container after editing
bind-mounted configuration too, so atomically replaced files are remounted; there
is no hot reload. Never copy a live bbolt file or run
two serving containers against it. No automatic migration, backup or update is
performed; retain the prior binary/image and offline backup until verification.

## Optional: build from source

For development, run from the repository root with Docker Buildx:

```sh
docker buildx build --platform linux/amd64 --load \
  --build-arg VERSION=dev --build-arg COMMIT="$(git rev-parse HEAD)" \
  --tag librecosta-uptime:dev-amd64 .
```

For arm64, replace both `amd64` occurrences with `arm64`. The Go 1.26 builder
cross-compiles on the build host without QEMU, using the same version/commit
injection as binary distributions. A full 40-digit SHA is required. Select this
local image in `IMAGE` when following the run examples. In PowerShell, assign
`$sha = git rev-parse HEAD` and use `--build-arg "COMMIT=$sha"`.
