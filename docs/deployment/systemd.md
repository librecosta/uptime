# systemd deployment

This example targets Linux system services (commands below suit Debian/Ubuntu).
Download the matching Linux archive and `SHA256SUMS` from
[GitHub Releases](https://github.com/librecosta/uptime/releases/tag/v0.1.0).
Use `amd64` for Intel/AMD x64 or `arm64` for ARM64 Linux.

## Download and verify

From a new download directory, use the architecture matching your server:

```sh
VERSION=v0.1.0
ARCH=amd64 # Use arm64 on ARM64 Linux.
ARCHIVE="librecosta-uptime_${VERSION}_linux_${ARCH}.tar.gz"
BASE="https://github.com/librecosta/uptime/releases/download/$VERSION"
curl --fail --location --remote-name "$BASE/$ARCHIVE"
curl --fail --location --remote-name "$BASE/SHA256SUMS"
sha256sum --check --ignore-missing SHA256SUMS && tar -xzf "$ARCHIVE"
curl --fail --location --output uptime.service \
  "https://raw.githubusercontent.com/librecosta/uptime/$VERSION/deploy/systemd/uptime.service"
```

Continue only after the archive checksum passes and all downloads succeed. The
archive contains `uptime` and `uptime.example.yaml`; the systemd unit is downloaded
separately from the same version. Review the unit before installing it.

## Install and configure

Create the dedicated account once, then install the binary and configuration:

```sh
sudo useradd --system --user-group --no-create-home \
  --home-dir /var/lib/librecosta-uptime --shell /usr/sbin/nologin librecosta-uptime
sudo install -m 0755 ./uptime /usr/local/bin/uptime
sudo install -d -m 0750 -o root -g librecosta-uptime /etc/librecosta-uptime
sudo install -m 0640 -o root -g librecosta-uptime \
  uptime.example.yaml /etc/librecosta-uptime/uptime.yaml
sudoedit /etc/librecosta-uptime/uptime.yaml
sudo install -m 0644 uptime.service /etc/systemd/system/librecosta-uptime.service
```

These are first-install steps; preserve existing configuration during upgrades.
Edit endpoints and keep `server.address: "127.0.0.1:18580"` for a local proxy.
The ordinary `storage.bbolt.path: ./data/uptime.db` resolves to
`/var/lib/librecosta-uptime/data/uptime.db`, **not** the YAML directory.
`StateDirectory=librecosta-uptime` creates and owns the working/state directory
for the service account, and makes it writable despite `ProtectSystem=strict`.
The account is not root; its private state directory and `UMask=0077` protect data.
When alerts are enabled in v0.2.0, `alerts.db` is created beside `uptime.db`, with
the same service ownership and mode 0600. No extra writable path is needed. Use
the [current example](../../configs/uptime.example.yaml) for candidate alert
configuration; the v0.1.0 download above does not include alerts. Run one alert
executor per database/Redis prefix and keep channel credentials private.
For a candidate, also install the unit from that candidate's source; its 45-second
stop budget replaces the older release unit's 30 seconds.

```sh
/usr/local/bin/uptime version
sudo -u librecosta-uptime /usr/local/bin/uptime config check \
  --config /etc/librecosta-uptime/uptime.yaml
sudo systemd-analyze verify /etc/systemd/system/librecosta-uptime.service
sudo systemctl daemon-reload
sudo systemctl enable --now librecosta-uptime
sudo systemctl status librecosta-uptime
sudo journalctl -u librecosta-uptime -n 50 --no-pager
curl --fail http://127.0.0.1:18580/livez
curl --fail http://127.0.0.1:18580/readyz
```

If you configure relative TLS paths, run configuration checks from the service's
working directory as well. Prefer absolute certificate/key paths outside home
directories; `ProtectHome=true` hides home locations. Give the service group read
and parent-directory traversal access to required TLS files. Do not loosen all
state permissions or disable hardening just to fix a path. Paths outside the
StateDirectory need explicitly reviewed writable exceptions if used for storage.

The unit leaves DNS, outbound HTTP(S) and Redis available, uses `PrivateTmp`,
`NoNewPrivileges` and no Linux capabilities. Non-privileged ports avoid needing
root; use a trusted TLS reverse proxy or deliberately configured native TLS with
Basic Auth for remote access. Plain HTTP Basic Auth does not encrypt credentials.
Health checks use the actual TLS scheme/hostname and trusted CA if TLS is enabled;
health remains unauthenticated. Target DOWN does not make the process unready.

## Stop, back up, maintain and upgrade

```sh
sudo systemctl stop librecosta-uptime
backup="/var/backups/librecosta-uptime/$(date -u +%Y%m%dT%H%M%SZ)"
sudo install -d -m 0700 /var/backups/librecosta-uptime
sudo mkdir -m 0700 "$backup"
sudo cp -a /var/lib/librecosta-uptime/data/. "$backup/"
sudo chown root:root "$backup"
sudo chmod 0700 "$backup"
sudo test -s "$backup/uptime.db"
# Also require this check if alerts have been enabled:
# sudo test -s "$backup/alerts.db"
```

Verify stop and backup succeed before proceeding; do not overwrite older backups.
Back up **both databases from the same stopped instance** whenever `alerts.db`
exists, including after alerts are disabled. Keep the matching YAML separately
and privately. To restore a matching backup set, stop the service and preserve
its current files first. If the backup predates alerts, use a fresh state directory
rather than retaining an unrelated, newer `alerts.db`. Restore service ownership:

```sh
sudo install -m 0600 -o librecosta-uptime -g librecosta-uptime \
  "$backup/uptime.db" /var/lib/librecosta-uptime/data/uptime.db
if sudo test -f "$backup/alerts.db"; then
  sudo install -m 0600 -o librecosta-uptime -g librecosta-uptime \
    "$backup/alerts.db" /var/lib/librecosta-uptime/data/alerts.db
fi
sudo systemctl start librecosta-uptime
curl --fail http://127.0.0.1:18580/readyz
```

Do not copy live bbolt files. There is no schema migration or automatic backup
task. With Redis, alert state uses the independent
`<storage.redis.key_prefix>:alerts:v1:` namespace in the same database; back up
monitoring and alert keys together through the Redis operator's RDB/AOF procedure.

Run offline bbolt maintenance with the **service account and working directory**:

```sh
sudo -u librecosta-uptime sh -c 'cd /var/lib/librecosta-uptime && exec /usr/local/bin/uptime service list --config /etc/librecosta-uptime/uptime.yaml'
```

Use the same invocation pattern for export/removal, following their existing
CLI safeguards. Export destinations need service-account write access. An export
is not a database backup; there is no archive import. Shared Redis still requires
coordination with other writers before deletion and has no cross-query snapshot.

Download and verify the new release for the same architecture before upgrading.
While stopped, replace the binary with the verified new Linux binary using
`sudo install -m 0755 /path/to/new/uptime /usr/local/bin/uptime`, preserving the
configuration and state directory. Then:

```sh
/usr/local/bin/uptime version
sudo systemctl start librecosta-uptime
sudo systemctl status librecosta-uptime
curl --fail http://127.0.0.1:18580/readyz
```

Use `sudo systemctl restart librecosta-uptime` after ordinary configuration changes;
run `daemon-reload` first when changing the unit. The service handles SIGTERM and
has a 45-second stop budget. HTTP/probe shutdown, observation draining and delivery
recording can use separate 10-second application timeout budgets; both workers
exit before alert storage closes, followed by monitoring storage. Increase
`TimeoutStopSec` when increasing `server.shutdown_timeout`. A systemd
stop timeout can still force termination; it cannot make blocked I/O safe.
Keep the previous binary and offline backup until the upgrade is verified.
There are no install/uninstall CLI commands, automatic upgrades or migrations.
