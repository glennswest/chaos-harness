# Structure and design rationale

This document explains *why* the harness is shaped the way it is.
It does not contain results, comparisons, or recommendations — only
the rationale for the structural choices.

## Process per component, not goroutine per component

The pathology this harness reproduces is what happens when many
**independent Go runtimes** run on one large host. Each runtime
makes its own decisions about:

* how many OS threads (M's) to spawn — controlled by `GOMAXPROCS`,
  which Go defaults to `runtime.NumCPU()` when there is no cgroup
  CPU quota;
* when to run garbage collection;
* what its scheduler does when it has more goroutines than M's.

A single Go process with N goroutines shares one scheduler, one
GC, one M-pool. That is not the failure mode. The failure mode
appears only when N separate runtimes each decide independently
that they own the whole machine.

So every "component" in the topology is a separate OS process
running its own copy of `chaos-worker`. There is no shared runtime
between components.

## Drift vs sync mode

Real OpenShift components do not reconcile in lockstep — each has
its own period, jitter, and startup offset. By default the harness
runs in **drift** mode: each worker chooses its reconcile cadence
independently. Over time, drift causes phase relationships between
workers to change.

**Sync** mode (`mode: sync` in the run config) forces all workers
to align their reconcile cycles via a Unix-socket sync server. This
is the worst-case alignment scenario — every worker hits its
reconcile burst inside the same wall-clock window.

The two modes bracket the real behaviour, which lives somewhere
between drift and forced sync.

## Victim isolation

A measurement workload (`chaos-victim`) runs alongside the chaos
fleet and produces an HDR histogram of scheduler wakeup jitter.

When no tuning is applied, the victim runs unconstrained — it sees
the full impact of the spike. When a PerformanceProfile is applied
(`performance_profile:` in the run config), the planner carves an
**exclusive** cpuset for the victim out of the isolated pool. Every
worker is then pinned to a slice of the workload pool that is
disjoint from the victim's CPUs. The invariant the planner enforces:
**the spike physically cannot reach the victim's CPUs.**

## Tuning subsystem

`pkg/tuning/` implements:

1. A PerformanceProfile parser (compatible with the OpenShift
   `performance.openshift.io/v2` shape, plus a `componentMap`
   extension this harness uses).
2. A preflight check that verifies the host satisfies the profile
   (RT kernel booted if required, isolcpus matches, irqbalance
   stopped, etc.).
3. A planner (`BuildPlan`) that produces an `Assignment` for every
   spawned process: cpuset, GOMAXPROCS, RT priority, memory cap.
4. A pluggable applier (`systemd-run`, `cgroup-v2`, `taskset`)
   that wraps each spawn in the chosen mechanism.

The planner is the part that enforces the disjoint-cpuset invariant.
The applier is what makes it stick at process-spawn time.

## Why six workload profiles

Real Go-runtime workloads cluster into a small number of behaviour
classes that stress different parts of the runtime:

| Class | Pressure |
|---|---|
| control-plane | goroutine count + channel signalling |
| networking | syscalls + short-lived goroutines |
| monitoring | allocation rate + channel ops |
| logging | very high allocation rate |
| operator-generic | low steady-state + periodic reconcile spike |
| etcd-like | fsync + heavy lock contention |

Six is enough to span the interesting failure modes without
overfitting to any specific component's quirks.

## The aggregator is intentionally minimal

`scripts/aggregate-results.py` uses only the Python stdlib by
default. PyYAML and matplotlib are optional. This keeps the harness
runnable on any RHEL 9.6 host without `pip install`.

The aggregator does not draw conclusions — it just produces:

* `summary.json` — structured headline numbers.
* `summary.md` — human-readable version of the same.
* `plot-threads.png` / `plot-victim.png` — optional visualisation.

Interpretation is left to the reader.

## What the harness does not do

* It does not run Kubernetes.
* It does not run containers.
* It does not measure the network stack — there is no traffic
  between workers (other than the optional Unix-socket sync).
* It does not compare CPUs, kernels, or distros — it just records
  what happens on whatever host you point it at.
