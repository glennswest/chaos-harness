# Output formats

Reference for the raw artefacts every run produces. The launcher
writes everything under `results/<run_id>/`.

## Per-run layout

```
results/<run_id>/
├── manifest.yaml          run + topology resolved spawn list
├── plan.yaml              tuning plan (only when performance_profile is set)
├── summary.json           aggregator structured output
├── summary.md             human-readable headline numbers
├── plot-threads.png       (matplotlib only)
├── plot-victim.png        (matplotlib only)
└── raw/
    ├── observer.csv       /proc time-series, 1 Hz
    ├── victim.hdr         HDR histogram snapshot of jitter
    ├── victim-buckets.csv per-second p50/p95/p99/p99.9 buckets
    └── worker-*.jsonl     per-worker event streams
```

## manifest.yaml

Captures the fully-resolved spawn list — every (component, replica)
plus the runtime parameters the launcher used. This is the ground
truth for "what did this run actually do?"

## plan.yaml

Only written when `performance_profile:` is set. Contains every
`Assignment` the planner produced: cpuset, GOMAXPROCS, RT priority,
memory cap, source explanation. Useful for diffing tuning decisions
between profiles.

## raw/worker-*.jsonl

One file per chaos-worker process, named
`worker-<profile>-<component>-<replica>-<pid>.jsonl`. Each line is
a single JSON event.

| `type` | Emitted | Notable fields |
|---|---|---|
| `startup` | Once at start | profile, component, replica_id, run_id, mode, duration_sec, gomaxprocs, num_cpu, affinity, cgroup_quota, cgroup_cpuset, go_version, version, pid |
| `sample` | Once per second | goroutines, threads, rss_bytes, gc_pause_ns_p99, gc_count |
| `gc_event` | After each GC cycle | cycles, pause_ns |
| `reconcile_start` / `reconcile_end` | Around each reconcile burst | (no extra fields beyond ts/type) |
| `shutdown` | Once at exit | reason |

Example (real output from a smoke run):

```jsonl
{"affinity":"0-15","cgroup_cpuset":"","cgroup_quota":"max 100000","component":"kubelet","duration_sec":60,"go_version":"go1.25.9","gomaxprocs":16,"mode":"drift","num_cpu":16,"pid":18849,"profile":"control-plane","replica_id":"kubelet-0","run_id":"run-smoke-baseline","ts":1778681003.955,"type":"startup","version":"dev"}
{"cycles":1,"pause_ns":5120,"ts":1778681003.996,"type":"gc_event"}
{"goroutines":213,"threads":211,"rss_bytes":84934656,"gc_pause_ns_p99":542000,"gc_count":12,"ts":1778681004.955,"type":"sample"}
{"ts":1778681030.001,"type":"reconcile_start"}
{"ts":1778681030.201,"type":"reconcile_end"}
{"ts":1778681063.456,"type":"shutdown","reason":"duration"}
```

## raw/observer.csv

Host-wide /proc time series, sampled at the rate in
`observer.sample_interval` (default 1 Hz):

```
ts, worker_count, total_threads, total_rss_mb,
loadavg_1, procs_running,
ctxt_per_sec, softirq_pct, irq_pct, steal_pct,
user_pct, system_pct, iowait_pct, idle_pct
```

`worker_count` and `total_threads` only count processes whose
`/proc/*/comm` and `cmdline` match `observer.worker_pid_filter`
(default `chaos-worker`).

## raw/victim.hdr

HDR histogram snapshot of the victim's wakeup-jitter distribution.
Two-column `value_ns count` per non-zero bucket. The HDR file is
the source of truth; `victim-buckets.csv` is a per-second
summary.

## raw/victim-buckets.csv

Per-second percentile rollup, one row per second:

```
ts,p50_us,p95_us,p99_us,p999_us,max_us,count
```

Useful for plotting jitter over time without parsing the HDR.

## summary.json

Aggregator output. Stable shape:

```json
{
  "run_id": "run2-headline",
  "headline": {
    "process_count": 55,
    "peak_total_threads": 0,
    "mean_total_threads": 0,
    "peak_aggregate_rss_mb": 0,
    "peak_loadavg_1": 0.0,
    "peak_ctxt_per_sec": 0,
    "alignment_events_per_min": 0.0,
    "alignment_events_aligned_seconds": 0,
    "victim_p99_us": 0,
    "victim_p99_us_max": 0,
    "victim_p999_us": 0,
    "victim_p999_us_max": 0,
    "victim_p99_baseline_multiplier": 0.0
  },
  "profile_breakdown": { "control-plane": 9, "networking": 4, "operator-generic": 33 },
  "per_worker": []
}
```

* `peak_total_threads` / `mean_total_threads` /
  `peak_aggregate_rss_mb` / `peak_loadavg_1` / `peak_ctxt_per_sec`
  — from `observer.csv`.
* `alignment_events_per_min` — count of 1-second windows in which
  ≥ 3 distinct workers were simultaneously in GC, normalised per
  minute. A proxy for the phase-lock pathology.
* `alignment_events_aligned_seconds` — raw count of those windows
  over the full run.
* `victim_p99_us` / `victim_p999_us` — overall percentiles from
  `victim.hdr`.
* `victim_p99_us_max` / `victim_p999_us_max` — worst per-second
  values from `victim-buckets.csv` (catches transient spikes the
  overall percentile smooths over).
* `victim_p99_baseline_multiplier` — only populated when the
  aggregator was given a baseline p99 via `--baseline-p99-us`.

## summary.md

Same numbers as `summary.json`, formatted as a small Markdown
table. Suitable for pasting into a notebook.

## plot-threads.png / plot-victim.png

Optional. Only generated when `matplotlib` is importable. The
threads plot overlays `total_threads` against the reconcile-burst
markers; the victim plot overlays per-second p99/p999 against the
same markers.
