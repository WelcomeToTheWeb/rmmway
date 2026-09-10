# Changelog

All notable changes to RMMWay will be documented in this file.

## [1.0.0] - 2026-09-10

### Added

- **Fleet dashboard** with per-client summary tiles (online, alerts, uptime, patch compliance)
- **Mobile-responsive UI** — the entire operator interface is usable on phones and tablets
- **Scheduled and compliance reports** in CSV and PDF:
  - Fleet status
  - Device report
  - Patch compliance
  - License compliance
  - Uptime/SLA
- **Maintenance windows** — pause alerting for devices, tags, or clients during planned work
- **Deep inventory collection** — hardware, software, services, users, domain membership
- **Patch management** — Windows Update query/approve/apply with third-party software support
- **Remote session** — live screen viewing of managed devices (view-only in v1.0.0)
- **User management & RBAC** — multiple operator accounts with role-based access and TOTP MFA
- **Ticketing** — alert-to-ticket escalation with assignment, SLA tracking, and resolution workflows
- **Multi-channel notifications** — email, Slack, Teams, PagerDuty with per-client routing policies
- **Cron flow triggers** — schedule automation runs
- **Load test harness** for verifying 5,000-device scale

### Changed

- Version bumped from 0.1.0 to 1.0.0
- Operator guide added (`docs/operator-guide.md`)
- Remote session documentation added (`docs/remote-session.md`)
- Release process documented (`RELEASE.md`)
- v1.0.0 release notes added (`docs/releases/v1.0.0.md`)

### Fixed

- Install.sh mTLS address derivation (both case branches strip scheme first)
- Device search index refresh on enroll and stream open

## [0.1.0] - 2026-09-08

### Added

- Initial release with fleet monitoring, dynamic baselining, alerts
- Agent enrollment with one-time tokens and mTLS
- Command dispatch, file transfer
- Self-healing playbooks and flow automation
- Client/tenant (MSP) model
- Webhooks, SSE events, client export
- Settings and profile pages

[1.0.0]: https://github.com/welcometotheweb/rmmway/releases/tag/v1.0.0
[0.1.0]: https://github.com/welcometotheweb/rmmway/releases/tag/v0.1.0
