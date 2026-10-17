# systemd deployment

This example targets Linux system services (commands below suit Debian/Ubuntu).
Obtain the matching Linux binary from a locally generated P7-A archive and verify
its SHA256SUMS first; no official release is implied. Keep the repository's
`configs/uptime.example.yaml` and `deploy/systemd/uptime.service` available.

## Install and configure

Create the dedicated account once, then install the binary and configuration:

```sh
sudo useradd --system --user-group --no-create-home \
  --home-dir /var/lib/librecosta-uptime --shell /usr/sbin/nologin librecosta-uptime
sudo install -m 0755 ./uptime /usr/local/bin/uptime
sudo install -d -m 0750 -o root -g librecosta-uptime /etc/librecosta-uptime
sudo install -m 0640 -o root -g librecosta-uptime \
  configs/uptime.example.yaml /etc/librecosta-uptime/uptime.yaml
sudoedit /etc/librecosta-uptime/uptime.yaml
sudo install -m 0644 deploy/systemd/uptime.service /etc/systemd/system/librecosta-uptime.service
```

These are first-install steps; preserve existing configuration during upgrades.
Edit endpoints and keep `server.address: "127.0.0.1:18580"` for a local proxy.
The ordinary `storage.bbolt.path: ./data/uptime.db` resolves to
`/var/lib/librecosta-uptime/data/uptime.db`, **not** the YAML directory.
`StateDirectory=librecosta-uptime` creates and owns the working/state directory
for the service account, and makes it writable despite `ProtectSystem=strict`.
The account is not root; its private state directory and `UMask=0077` protect data.

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
sudo install -d -m 0700 /var/backups/librecosta-uptime
sudo cp --preserve=mode,timestamps /var/lib/librecosta-uptime/data/uptime.db \
  "/var/backups/librecosta-uptime/uptime-$(date -u +%Y%m%dT%H%M%SZ).db"
```

Verify stop and backup succeed before proceeding; do not overwrite older backups.
Run offline bbolt maintenance with the **service account and working directory**:

```sh
sudo -u librecosta-uptime sh -c 'cd /var/lib/librecosta-uptime && exec /usr/local/bin/uptime service list --config /etc/librecosta-uptime/uptime.yaml'
```

Use the same invocation pattern for export/removal, following their existing
CLI safeguards. Export destinations need service-account write access. An export
is not a database backup; there is no archive import. Shared Redis still requires
coordination with other writers before deletion and has no cross-query snapshot.

While stopped, replace the binary with a verified new Linux binary using
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
has 30 seconds to stop, exceeding the default 10-second application timeout.
Increase `TimeoutStopSec` when increasing `server.shutdown_timeout`. A systemd
stop timeout can still force termination; it cannot make blocked I/O safe.
Keep the previous binary and offline backup until the upgrade is verified.
There are no install/uninstall CLI commands, automatic upgrades or migrations.
