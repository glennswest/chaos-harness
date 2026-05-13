# Tuning configuration reference

How to wire the harness to an OpenShift-style PerformanceProfile,
and which knobs change between vendors.

## When to use a tuning profile

Out of the box, the harness runs every chaos-worker, the victim, and
the observer as unconstrained processes. That is the configuration
that exhibits the failure mode this harness exists to reproduce.

A PerformanceProfile changes that: the launcher pins every spawned
process to a cpuset and forces its `GOMAXPROCS` to the cpuset width.
Use a tuning profile when you want to measure the **mitigation** —
"what does the spike look like once each Go runtime sees only a
fraction of the host's CPUs?"

## How to apply a profile

In a run config:

```yaml
performance_profile: examples/profiles/large-host-rt.yaml
tuning_backend: ""            # auto-select; usually systemd-run
```

The launcher will:

1. Parse the profile.
2. Run preflight against the live host (kernel cmdline, irqbalance
   state, RT kernel presence, etc.).
3. Build a tuning plan that maps every (component, replica) to a
   cpuset assignment.
4. Apply the plan via the chosen backend.
5. Spawn each process inside its slice.

To inspect the plan without running anything:

```sh
./bin/chaos-tuned explain --profile examples/profiles/large-host-rt.yaml \
                          --topology sno
```

`chaos-tuned` has five subcommands:

| Subcommand | Purpose |
|---|---|
| `preflight` | Check live host against `--profile` only — no topology needed |
| `plan` | Emit the resolved plan as YAML or JSON for offline review |
| `explain` | Human-readable per-process plan (cpuset, GOMAXPROCS, source) |
| `apply` | Apply the plan via a chosen backend (without spawning workers) |
| `verify` | Compare a running fleet's actual placement against the plan |

## Configuration knobs

### Per-vendor: the c-state cmdline arg

Most of an RT-host kernel cmdline is portable. The only vendor-
specific knob is the deep-idle / c-state setting:

| Vendor | additionalKernelArgs |
|---|---|
| Intel | `intel_idle.max_cstate=0` + `processor.max_cstate=1` |
| AMD and other non-Intel | `processor.max_cstate=1` |

`intel_idle.max_cstate` is silently ignored on non-Intel hosts but
emits a kernel warning on some kernels. Leave it off unless you
know you are on an Intel platform.

The shipped `examples/profiles/large-host-rt.yaml` defaults to the
AMD-compatible cmdline and includes a commented-out Intel line you
can uncomment.

### Per-topology: cpu.reserved / cpu.isolated

These two fields define the partition between OS / management cores
and workload cores:

```yaml
spec:
  cpu:
    reserved: "0-7"
    isolated: "8-287"
```

Rules of thumb:

* Reserve at least one full NUMA-local block (typically 4–8 cores
  on the first socket / first chiplet) for the OS, kubelet, and
  observer.
* Everything else goes to `isolated`. The chaos workers, the
  isolated-shared data plane, and the victim all carve out of this
  pool.

### Per-host: victim cpuset

The victim must sit on CPUs no worker can reach. The default
profiles place it at the top of the `isolated` range. The right
choice depends on cache topology:

* **Monolithic CPUs (no chiplets) / 2-socket boxes** — pick 4 CPUs
  at the top of the last NUMA node so the victim shares LLC only
  with itself.
* **Chiplet CPUs (multiple CCDs / cache complexes)** — pick all 4
  CPUs from a single chiplet so they share one last-level cache
  and never cross-CCD-fetch.
* **SMT enabled** — include both SMT siblings of those cores. If
  you reserve only the primary thread, a workload can still land
  on the sibling thread and steal L1/L2 + execution units.

Example for a 160-core / 320-thread chiplet host, where each
chiplet is 16 cores and SMT is enabled:

```yaml
chaos-victim:
  class: isolated-exclusive
  cpus: "156-159,316-319"   # top 4 cores of last chiplet, both SMT siblings
  rtPriority: 50
```

### Per-class: componentMap

`componentMap` decides where each topology component lands. Four
classes:

| Class | Pool | Exclusive | Typical use |
|---|---|---|---|
| `reserved` | reserved cores | shared | kubelet, control plane, observer |
| `isolated-shared` | workload pool | shared | data-plane daemonsets (OVN-K, multus) |
| `burstable` | workload pool | shared | monitoring, logging |
| `best-effort` | workload pool | shared | generic operators |
| `isolated-exclusive` | workload pool | **disjoint** | chaos-victim, dedicated low-jitter workloads |

`defaultClass:` sets the fallback for any component not listed in
`componentMap`.

## Tuning backends

| Backend | Mechanism | Requirements |
|---|---|---|
| `systemd-run` | per-process transient unit | systemd, root or appropriate polkit |
| `cgroup-v2` | direct cgroup writes | cgroup v2 unified hierarchy, root |
| `taskset` | sched_setaffinity only | none (no GOMAXPROCS forcing) |

Leave `tuning_backend: ""` to auto-select. `systemd-run` is preferred
because it gives the planner cgroup-level enforcement of GOMAXPROCS
(via CPU quota) without requiring direct cgroup writes.

## Preflight

The launcher refuses to apply a profile that does not match the live
host. Common preflight failures and what they mean:

| Failure | Meaning | Fix |
|---|---|---|
| `realTimeKernel.enabled but uname -r missing rt` | Profile asks for RT, host isn't booted into kernel-rt | Boot kernel-rt or use a non-RT profile |
| `isolcpus mismatch` | Kernel cmdline doesn't isolate the right range | Update GRUB cmdline and reboot |
| `irqbalance is running` | Profile bans IRQ load-balancing, host hasn't stopped irqbalance | `systemctl stop irqbalance` |
| `cgroup v1 detected` | Backend requires unified hierarchy | Switch host to cgroup v2 |
| `componentMap references unknown component "X"` | Typo or topology mismatch | Fix the name |

Run `chaos-tuned preflight --profile <path>` to check a profile
against the live host without spawning anything.
