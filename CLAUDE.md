# CLAUDE.md — chaos-harness

Project-specific guidance for Claude when working in this repo.

## What this project is

A bare-metal Linux chaos harness that reproduces the
multi-Go-process `GOMAXPROCS` thread-explosion failure mode and
measures its impact on a victim workload. See `README.md` for the
full story.

This is the **public** version of the harness. Internal reports,
customer hardware references, and result write-ups live elsewhere
and must not be added back here.

## Module path

`github.com/glennswest/chaos-harness`

## Layout

```
cmd/            five entry points (launcher, worker, victim, observer, tuned)
pkg/            shared packages (profile, runconfig, topology, tuning, ...)
topologies/     YAML topologies (sno, master, worker, future-bloat)
examples/       PerformanceProfile templates
test-matrix/    canonical run configurations
scripts/        aggregate-results.py, run-matrix.sh
docs/           STRUCTURE.md, TUNING.md, OUTPUTS.md
```

## What belongs in `docs/`

* Structural rationale (why the design works the way it does).
* Configuration references (how to set knobs).
* Output format references.

## What does NOT belong in `docs/`

* Test reports.
* Result write-ups.
* Vendor comparisons (Intel vs AMD, kernel-X vs kernel-Y, etc.).
* Dated analysis files.

If a question of the form "how did this run go?" comes up, the
answer lives in the user's run outputs, not in this repository.

## Build and test

```sh
make build               # all five binaries
make test
make vet
make matrix-quick        # 60 s smoke run
```

## When asked to add a new test run

* Add a `test-matrix/run-*.yaml`.
* Add a row to the "Test matrix" table in `README.md`.
* Add an entry to `CHANGELOG.md`.

## When asked to add a new PerformanceProfile

* Add a template under `examples/profiles/`.
* The template must be vendor-neutral by default. If a knob differs
  between vendors (e.g. the c-state cmdline), document both choices
  in a header comment.

## Cross-project rules

This project lives under a parent directory governed by
`../CLAUDE.md`: conventional commits, immediate push, changelog
discipline, semver.
