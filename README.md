# chaos-harness

Aggregate-node `GOMAXPROCS` chaos harness for bare-metal Linux.

Reproduces the failure mode where many unconstrained Go processes on
one large host simultaneously over-provision `GOMAXPROCS = NumCPU`
and phase-lock their reconcile cycles, producing thread explosion
and host-wide scheduler tail latency. The harness measures the
impact on a well-behaved victim workload running on the same host.

This repository contains the runnable harness. It does **not** ship
test results, comparisons, or recommendations — only the binaries,
the configuration knobs, and the raw outputs.

---

## What this is, and what it isn't

**Is:** a single-host simulation that spawns a realistic
OpenShift-style component layout (SNO, multinode-master, or
multinode-worker) as a fleet of independent Go processes, drives them
with workload profiles tuned to mirror real Go-runtime workload
classes, and measures the impact on a victim workload running on the
same host.

**Isn't:** a Kubernetes cluster. There is no kubelet, crio, or
container runtime. Each "OpenShift component" is a `chaos-worker`
process simulating that component's *Go-runtime workload profile* —
allocation rate, channel ops, syscall pressure, lock contention,
reconcile cadence. This is deliberate: the failure mode is rooted
in `GOMAXPROCS × NumCPU` and per-process M-creation, not in
Kubernetes itself.

---

## Why a fleet of processes (and not goroutines)?

The pathology only appears when **independent Go runtimes** contend
for one host's resources. A single Go process with N goroutines
shares one runtime, one scheduler, one GC, and one M-pool — that's
not the bug. The bug is what happens when 30+ separate runtimes
each decide independently that they own all the CPUs:

* every process spawns up to `NumCPU` OS threads (M's),
* every process schedules its own GC,
* every process runs its own reconcile loop,
* and they drift into phase alignment, so a coordinated GC
  stop-the-world lands inside the same wall-clock window across
  the whole fleet.

So `chaos-worker` is its own Go process — never a goroutine in a
shared runtime. Process isolation is the whole point.

---

## Architecture

```
chaos-launcher
  ├─ reads run-config YAML + topology YAML
  ├─ optionally loads a PerformanceProfile and computes a tuning plan
  ├─ writes manifest.yaml
  ├─ spawns chaos-observer    (1 instance, /proc sampler)
  ├─ spawns chaos-victim      (1 instance, jitter measurement)
  ├─ spawns chaos-worker × N  (one per (component, replica) in topology)
  ├─ optional sync server     (Unix socket — sync-mode reconcile)
  ├─ waits for duration
  ├─ SIGTERM all (10 s grace, then SIGKILL)
  └─ invokes scripts/aggregate-results.py
```

The five binaries:

| Binary | Role |
|---|---|
| `chaos-launcher` | Reads run-config, applies tuning, spawns the fleet, aggregates outputs |
| `chaos-worker` | One per simulated component; runs a workload profile |
| `chaos-victim` | High-resolution scheduler-jitter probe |
| `chaos-observer` | /proc sampler — total threads, RSS, ctxt, IRQ stats |
| `chaos-tuned` | Standalone tuning-plan inspector (computes what the launcher would apply) |

---

## Build

Requires Go 1.24 or 1.25. Go 1.25 is only needed if you intend to
run `run7-go125-no-cgroup`, which deliberately exercises Go 1.25's
`GOMAXPROCS` behaviour without a cgroup quota.

```sh
make build               # builds all five binaries into bin/
make build-go125         # additional: bin/chaos-worker-go125 (run 7)
make test
make vet
```

The harness builds and runs on Linux amd64 and arm64. macOS and
other developer hosts build but lack `/proc`, `sched_getaffinity`,
and `clock_nanosleep` — measurements there are for development only.

For a complete end-to-end setup from a fresh RHEL 9.6 install
through a booted-and-tuned RT host running the headline tuned run,
see [`docs/SETUP.md`](docs/SETUP.md). That document walks through
the kernel-rt install, GRUB cmdline editing (including the
vendor-specific c-state knob choice), isolation verification,
cgroup-v2 confirmation, and the smoke + tuned-RT runs.

---

## Quick start

```sh
make build
./bin/chaos-launcher --config test-matrix/run2-headline.yaml --output-dir results/
```

When the launcher exits you'll find:

```
results/run2-headline/
├── manifest.yaml          run + topology resolved spawn list
├── summary.json           aggregator structured output
├── summary.md             human-readable headline numbers
├── plot-threads.png       (if matplotlib installed)
├── plot-victim.png        (if matplotlib installed)
└── raw/
    ├── observer.csv       /proc time-series, 1 Hz
    ├── victim.hdr         HDR histogram snapshot of jitter
    ├── victim-buckets.csv per-second p50/p95/p99/p99.9 buckets
    └── worker-*.jsonl     per-worker event streams
```

To re-aggregate without re-running:

```sh
make aggregate RUN=run2-headline
```

---

## Run configuration

Run configs live in `test-matrix/`. Schema:

```yaml
run_id: my-experiment              # required; embedded in output paths
duration: 600s                     # default 10m
mode: drift                        # drift | sync
topology: sno                      # sno | master | worker | future-bloat | path/to/custom.yaml

# Per-process GOMAXPROCS override. 0 means honor topology values
# (which themselves default to 0 = inherit Go's default behaviour,
# which IS the failure mode this harness reproduces).
worker_gomaxprocs: 0               # set to e.g. 4 for the mitigation runs

# Optional PerformanceProfile to apply via the tuning subsystem.
# When set, the launcher pins every spawned process to a cpuset and
# forces its GOMAXPROCS to the cpuset width.
performance_profile: ""            # e.g. examples/profiles/large-host-rt.yaml
tuning_backend: ""                 # ""=auto, "systemd-run", "cgroup-v2", "taskset"

# Only honoured when mode: sync.
sync_trigger:
  initial_offset: 30s              # let workers spin up first
  period: 15s                      # cadence of forced alignment

victim:
  mode: hires_jitter               # hires_jitter | http_rtt
  pinning_cpus: "0-3"              # empty = unconstrained (tuning plan overrides)

observer:
  sample_interval: 1s
  worker_pid_filter: chaos-worker  # /proc/*/comm + cmdline match
```

---

## Topology selection

The `topology` field points to either a built-in name or a YAML
path. Built-ins under `topologies/`:

| Name | Components | Processes | Use |
|---|---|---|---|
| `sno` | 55 | 55 | Single-Node OpenShift — control plane + cluster operators + monitoring + logging + workload all on one host |
| `master` | 33 | 33 | One master in a 3-master cluster — local etcd, apiserver, controller-manager, scheduler, operator replicas, plus node-side agents |
| `worker` | 14 | 29 | Multinode worker — kubelet, OVN-K node, CSI, monitoring/logging daemonsets, and a tunable user-workload bucket |
| `future-bloat` | ~35 | ~60 | SNO baseline + 25 illustrative future operator processes |

Each topology lists components → processes with profile, replica
count, GOMAXPROCS, and `ideal_threads` documentation. Edit the YAML
to taste; pull the file path into a run config to use a custom one.

---

## Workload profiles

Each process is one of six profiles (`pkg/profile/`):

| Profile | Models | Steady-state | Reconcile burst |
|---|---|---|---|
| `control-plane` | kube-apiserver / controller-manager / scheduler | 200 goroutines, 10 MB/s alloc, heavy channel signalling | 30 s, 200 ms burst, 5× alloc, +500 goroutines |
| `networking` | OVN-K / multus / CNI | 80 goroutines, 5k syscalls/s loopback TCP | 15 s, 100 ms burst, 3× syscalls, +200 goroutines |
| `monitoring` | Prometheus / Thanos | 100 goroutines, 5 MB/s alloc, 20k channel ops/s | 30 s, 500 ms burst, 10× alloc |
| `logging` | Vector / Loki | 150 goroutines, **50 MB/s alloc**, 100k channel ops/s | 10 s, 200 ms burst, 2× alloc |
| `operator-generic` | Platform/customer operators | 50 goroutines, 1 MB/s alloc | 30 s, 100 ms burst, 2× alloc |
| `etcd-like` | etcd | 100 goroutines, **fsync-heavy**, high lock contention | 10 s, 50 ms burst, 2× syscalls |

Numbers are starting values; tune via custom topology YAML.

---

## Test matrix

The canonical runs in `test-matrix/`:

| Run | Mode | GOMAXPROCS | Tuning profile | Purpose |
|---|---|---|---|---|
| `run1-baseline-64core` | drift | default | none | Baseline node chaos on a 64–128 core host |
| `run2-headline` | drift | default | none | Headline number on a large host |
| `run3-sync-worst` | sync | default | none | Worst-case forced alignment |
| `run4-mitigation` | drift | 4 | none | GOMAXPROCS-only mitigation |
| `run5-mitigation-sync` | sync | 4 | none | Mitigation under sync |
| `run6-future-bloat` | drift | default | none | Future-bloat (extra operators) |
| `run7-go125-no-cgroup` | drift | default | none | Go 1.25 binary, no cgroup quota |
| `run8-tuned` | drift | planner | `sno-telco` | Full PerformanceProfile mitigation |
| `run9-tuned-sync` | sync | planner | `sno-telco` | Tuned under sync |
| `run-smoke-baseline` | drift | default | none | 60 s smoke on a worker topology |
| `run-smoke-tuned` | drift | planner | `smoke-16cpu` | 60 s smoke with tuning |
| `run-tuned-rt` | drift | planner | `large-host-rt` | Large host RT-tuned mitigation |

Run the full matrix:

```sh
make matrix              # full duration (10 min × N)
make matrix-quick        # 60 s smoke run
make run RUN=run2-headline
```

Selectively:

```sh
DURATION=60s ONLY="run1-baseline-64core run2-headline" \
    ./scripts/run-matrix.sh
```

---

## Configuring for your host (including Intel vs AMD)

The harness itself does not care about the CPU vendor. What does
care is the kernel cmdline of an RT-tuned host. The deep-idle /
c-state knob differs by vendor; everything else is portable.

### Pick the c-state knob for your vendor

In `additionalKernelArgs:` of a PerformanceProfile:

| Vendor | Kernel cmdline |
|---|---|
| Intel | `intel_idle.max_cstate=0` **and** `processor.max_cstate=1` |
| AMD / non-Intel | `processor.max_cstate=1` only |

`intel_idle.max_cstate` is silently ignored on non-Intel hosts but
emits a kernel warning on some kernels — leave it off unless you
know you are on an Intel platform.

`examples/profiles/large-host-rt.yaml` ships with the AMD-compatible
default and a commented-out Intel line — uncomment for Intel hosts.

### Pick the victim cpuset for your topology

The harness's invariant is: **the spike must not reach the victim's
CPUs.** Choose the victim cpuset based on your hardware:

* **Monolithic / 2-socket CPUs** — pick 4 CPUs at the top of the
  last NUMA node (e.g. on a 2×144-core box: `cpus: "284-287"`).
* **Chiplet CPUs (multiple CCDs / cache complexes)** — pick all 4
  CPUs from a single chiplet so they share one last-level cache.
* **SMT enabled** — include both SMT siblings of those cores so no
  workload can land on the sibling thread and steal L1/L2 or
  execution units. For a 160-core / 320-thread chiplet host, both
  sibling sets of the top 4 cores of the last CCD look like
  `cpus: "156-159,316-319"`.

### Pick the isolated range for your core count

`cpu.reserved` and `cpu.isolated` must add up to (or be a subset of)
your host's logical CPU count. The supplied templates assume
`0-7` reserved and the remainder isolated; resize both ranges to
match your box, and update `chaos-victim.cpus` to sit at the top of
the resized isolated range.

---

## Output formats

### Worker JSONL (one event per line)

```jsonl
{"affinity":"0-15","cgroup_quota":"max 100000","component":"kubelet","duration_sec":60,"go_version":"go1.25.9","gomaxprocs":16,"mode":"drift","num_cpu":16,"pid":18849,"profile":"control-plane","replica_id":"kubelet-0","run_id":"run-smoke-baseline","ts":1778681003.955,"type":"startup","version":"dev"}
{"cycles":1,"pause_ns":5120,"ts":1778681003.996,"type":"gc_event"}
{"goroutines":213,"threads":211,"rss_bytes":84934656,"gc_pause_ns_p99":542000,"gc_count":12,"ts":1778681004.955,"type":"sample"}
{"ts":1778681030.001,"type":"reconcile_start"}
{"ts":1778681030.201,"type":"reconcile_end"}
{"ts":1778681063.456,"type":"shutdown","reason":"duration"}
```

Easy to grep, easy to feed `jq`, easy to ingest in pandas. See
[`docs/OUTPUTS.md`](docs/OUTPUTS.md) for the full event-type table.

### Observer CSV columns

```
ts, worker_count, total_threads, total_rss_mb,
loadavg_1, procs_running,
ctxt_per_sec, softirq_pct, irq_pct, steal_pct,
user_pct, system_pct, iowait_pct, idle_pct
```

### Victim outputs

- `victim.hdr` — HDR snapshot, two-column `value_ns count` per
  non-zero bar.
- `victim-buckets.csv` — per-second
  `ts,p50_us,p95_us,p99_us,p999_us,max_us,count`.

### Aggregator headline (summary.json)

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

`alignment_events_per_min`: count of 1-second windows in which
≥ 3 distinct workers are simultaneously in GC. Robust proxy for the
phase-lock pathology. See [`docs/OUTPUTS.md`](docs/OUTPUTS.md) for
the rest of the headline fields.

---

## Hardware requirements

| Run | Cores (logical) | RAM |
|---|---|---|
| matrix-quick smoke | 4+ | 4 GB |
| run1-baseline-64core | 64–128 | 16 GB |
| run2 / run3 / run4 / run5 / run6 / run7 | 192+ | 32 GB |

Disk: ~1 GB per full matrix run. Network: none.

---

## Aggregator dependencies

The aggregator script uses only the Python stdlib, so it runs
without `pip install`. Two optional packages give you more:

- **PyYAML** — needed to parse `manifest.yaml` for richer summaries.
- **matplotlib** — needed to emit `plot-threads.png` and
  `plot-victim.png`.

```sh
pip install --user pyyaml matplotlib
# or:  dnf install python3-pyyaml python3-matplotlib
```

---

## Comparing runs (baseline multiplier)

To express a degraded run's victim p99 as a multiple of a baseline
run's:

```sh
BASELINE=$(jq -r '.headline.victim_p99_us' results/baseline/summary.json)
python3 scripts/aggregate-results.py \
    --run-dir results/run2-headline \
    --baseline-p99-us $BASELINE
```

The headline now includes `victim_p99_baseline_multiplier`.

---

## License

Apache-2.0. See `LICENSE`.
