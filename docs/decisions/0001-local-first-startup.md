# 0001: Loopback defaults and first access

Date: 2026-10-15  
Status: **Accepted** (P5.5)

## Context

The original design defaulted to `:8080` and the implemented root returned 404.
With authentication disabled by default, first startup exposed a status service
on all interfaces while leaving a user visiting `/` without a dashboard entry.

## Decision

Omitting `server.address` now selects `127.0.0.1:18580`. Keep all existing explicit
host:port formats and values. Deployments requiring public, container, or remote
proxy access can explicitly select `:18580`; a same-host proxy may use loopback.
The official example matches the new default.

GET and HEAD `/` return a temporary HTTP 302 to the validated `ui.path`. Do not
carry incoming query parameters. Keep dashboard/API authentication at the target
namespace and preserve health routes, other methods, and unmatched-route behavior.

After binding and initializing Uptime, the OnListen log identifies LibreCosta
Uptime, the actual bound address, HTTP/HTTPS, and the dashboard entry. HTTP on a
concrete address supplies a URL; wildcard binds use a host placeholder. TLS uses
a certificate-host placeholder because a listener address cannot establish a
certificate identity or trust. Do not print configuration or target/Redis URLs.

## Consequences and alternatives

Configurations that omitted the address must explicitly opt into external access
and update port forwarding. Existing explicit `:8080` configurations still work.
There is no automatic fallback if port 18580 is occupied. Keeping an all-interface
default was rejected to make exposure intentional; 18580 is an unprivileged
product default, not a guarantee against port conflicts.

A 302 allows the configured dashboard location to change without a permanent
redirect. A welcome page or custom frontend would add maintenance without helping
the direct first-access path. Storage identities, Schema v1, ownership, and
shutdown behavior are unaffected. This decision supersedes the original design's
listener default; the design remains a historical baseline for future phases.
