# Changelog

## [v0.1.0] — 2026-05-13

Initial public release.

### Added
- Five binaries: `chaos-launcher`, `chaos-worker`, `chaos-victim`,
  `chaos-observer`, `chaos-tuned`.
- Six workload profiles (control-plane, networking, monitoring,
  logging, operator-generic, etcd-like).
- Four built-in topologies (sno, master, worker, future-bloat).
- PerformanceProfile-driven tuning subsystem with three backends
  (systemd-run, cgroup-v2, taskset) and `preflight` / `plan` /
  `explain` / `apply` / `verify` subcommands on `chaos-tuned`.
- HDR-histogram-based jitter probe in `chaos-victim`.
- Canonical test matrix in `test-matrix/` (smoke, baseline,
  headline, sync, mitigation, tuned-RT, Go 1.25 variants).
- Generic example PerformanceProfile templates:
  `examples/profiles/large-host.yaml`,
  `examples/profiles/large-host-rt.yaml`,
  `examples/profiles/sno-telco.yaml`,
  `examples/profiles/smoke-16cpu.yaml`,
  `examples/profiles/smoke-2cpu.yaml`.

### Documentation
- README.
- `docs/STRUCTURE.md` — design rationale.
- `docs/TUNING.md` — tuning subsystem reference, including the
  per-vendor c-state cmdline knob choice.
- `docs/OUTPUTS.md` — raw output format reference.
- `docs/SETUP.md` — end-to-end setup for **RHEL 9.6** from a fresh
  install through a tuned-RT run.

### Notes
- Module path: `github.com/glennswest/chaos-harness`.
- `docs/SETUP.md` describes the same RHEL 9.6 procedure the harness
  was originally exercised against internally. The instructions are
  provided as documentation; they have not been independently
  re-validated against a fresh RHEL 9.6 install in this public tree.
