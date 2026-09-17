# Security Policy

## This is a honeypot

This repo is an intentionally vulnerable-looking MySQL honeypot. Do not run
the stock configuration against real data, and do not probe public
deployments without permission.

## Reporting a vulnerability

Do not open a public issue for security-sensitive bugs. Instead, open a
private security advisory on GitHub or contact the maintainer directly.
Include steps to reproduce, impact, and the commit hash.

Please do not attack live honeypot instances to demonstrate an issue; use a
local `docker compose up --build` reproduction.

## Operational notes for deployers

- Honeypot credentials are hardcoded in `cmd/seed-redis/seed_data.yaml`
  (`mysql_users`). Change them before any public deployment.
- All seed data is synthetic and fictional; do not treat it as a real leak.
- Keep Elasticsearch/Kibana/Redis on localhost only (default compose
  bindings). `xpack.security.enabled=false` is local-dev only — never expose
  port 9200/5601 publicly.
- Only `3306/tcp` (and `22/tcp` for admin) should be reachable; see README
  firewall section.
- Change obvious fingerprints in production: version string
  (`handleSelectVersion`), DB/table names, Vector index/label.
