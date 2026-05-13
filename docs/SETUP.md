# Setup — from a fresh RHEL 9.6 install to a tuned-RT chaos run

End-to-end setup for a **RHEL 9.6** host with an active
subscription. The commands and package names below are written
against RHEL 9.6 specifically; other distributions are out of scope
for this document.

> **Validation status.** The procedure described here is derived
> from the same workflow used to run the harness internally on a
> RHEL 9.6 host. It is provided as documentation only — the
> commands have not been re-validated end-to-end in this public
> tree against a fresh RHEL 9.6 install. Treat each step as a
> waypoint to confirm rather than a guarantee. If your host's
> behaviour diverges from what's described, the authoritative
> checks are `uname -r`, `/proc/cmdline`, the contents of
> `/sys/devices/system/cpu/{isolated,nohz_full}`, and
> `chaos-tuned preflight`.

If you only want to build and smoke-test the harness without an
RT-tuned host, you can skip straight to **6. Build the harness** and
**8. Smoke run (no tuning)**. Everything else is the tuned-RT path.

---

## 0. Hardware sanity check

Minimum to reproduce the failure mode meaningfully:

| Run kind | Logical CPUs | RAM |
|---|---|---|
| smoke (`make matrix-quick`) | 4+ | 4 GB |
| baseline 64-core | 64–128 | 16 GB |
| headline | 192+ | 32 GB |

Check:

```sh
nproc
lscpu | grep -E '^CPU\(s\):|Socket|NUMA|Model name'
free -h
```

Note your CPU vendor (`lscpu | grep "Vendor ID"`) — you will need it
in step 3 to pick the right c-state cmdline arg.

---

## 1. Prerequisites — packages

Install build tools, Python, and observability prerequisites:

```sh
sudo dnf install -y \
  git make gcc \
  python3 python3-pyyaml python3-matplotlib \
  tuned tuned-profiles-realtime \
  systemd-libs jq
```

Verify cgroup v2 is the unified hierarchy (it is, by default, on
RHEL 9 and the listed downstreams):

```sh
stat -fc %T /sys/fs/cgroup
# expect: cgroup2fs
```

If it reports `tmpfs`, the host is on cgroup v1. To switch:

```sh
sudo grubby --update-kernel=ALL \
    --args="systemd.unified_cgroup_hierarchy=1"
sudo reboot
```

Then re-verify after reboot.

---

## 2. Prerequisites — Go toolchain

The harness requires Go 1.24 or 1.25. RHEL 9.6's `golang` package
typically lags. Install upstream Go:

```sh
GO_VERSION=1.24.4
curl -LO https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go${GO_VERSION}.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' | sudo tee /etc/profile.d/go.sh
source /etc/profile.d/go.sh
go version
```

For arm64 hosts, substitute `linux-arm64`.

For `run7-go125-no-cgroup` you also need Go 1.25 side-by-side. The
simplest way is the upstream installer-as-binary path:

```sh
go install golang.org/dl/go1.25@latest
$HOME/go/bin/go1.25 download
# Make it visible to the Makefile as `go1.25`:
sudo ln -s $HOME/go/bin/go1.25 /usr/local/bin/go1.25
```

---

## 3. Kernel — install kernel-rt

The non-tuned baseline runs (`run1` through `run7`) need only the
stock RHEL 9.6 kernel. The tuned-RT runs (`run-tuned-rt`, `run8`,
`run9`) require the RT kernel from the RHEL 9 real-time repository.

Enable the repo and install the RT kernel:

```sh
sudo subscription-manager repos --enable=rhel-9-for-x86_64-rt-rpms
sudo dnf install -y kernel-rt kernel-rt-core
```

Verify the RT kernel is now an available boot entry:

```sh
sudo grubby --info=ALL | grep -E 'title|kernel'
```

Set the RT kernel as default (replace the path with whichever
`kernel-rt-...` entry `grubby --info=ALL` showed):

```sh
sudo grubby --set-default /boot/vmlinuz-$(rpm -q --queryformat '%{VERSION}-%{RELEASE}.%{ARCH}\n' kernel-rt | head -1)+rt
```

(Do **not** reboot yet — first add the cmdline args in step 4.)

---

## 4. Kernel cmdline — isolation + idle knobs

The RT kernel needs cmdline arguments to isolate the workload pool
from housekeeping IRQs, ticks, and RCU callbacks. Pick the
`isolcpus=` range to match what your `PerformanceProfile` will set
as `cpu.isolated`.

### 4a. Pick the c-state knob for your vendor

| Vendor | Required args |
|---|---|
| Intel | `intel_idle.max_cstate=0` **and** `processor.max_cstate=1` |
| AMD and other non-Intel | `processor.max_cstate=1` |

`intel_idle.max_cstate` is silently ignored on non-Intel hosts but
emits a kernel warning on some kernels — leave it off unless you
are on an Intel platform.

### 4b. Apply the cmdline

Assuming an `isolcpus` range of `8-287` (adjust to your host's
logical CPU count minus the reserved range):

**On AMD / non-Intel:**

```sh
sudo grubby --update-kernel=ALL \
    --args="isolcpus=managed_irq,8-287 \
nohz_full=8-287 \
rcu_nocbs=8-287 \
processor.max_cstate=1 \
idle=poll \
skew_tick=1 \
nowatchdog"
```

**On Intel:**

```sh
sudo grubby --update-kernel=ALL \
    --args="isolcpus=managed_irq,8-287 \
nohz_full=8-287 \
rcu_nocbs=8-287 \
intel_idle.max_cstate=0 \
processor.max_cstate=1 \
idle=poll \
skew_tick=1 \
nowatchdog"
```

Verify what `grubby` will boot with:

```sh
sudo grubby --info=DEFAULT
```

The `args` line should contain everything you just added.

### 4c. Reboot into kernel-rt

```sh
sudo reboot
```

---

## 5. Post-reboot verification

After reboot, verify every piece of the configuration:

```sh
# (a) RT kernel booted
uname -r
# expect: ...rt... in the kernel release string

# (b) cmdline contains every isolation knob you added
cat /proc/cmdline
# expect: contains isolcpus=managed_irq,8-287 nohz_full=8-287
#                  rcu_nocbs=8-287 processor.max_cstate=1 ...

# (c) isolcpus took effect at runtime
cat /sys/devices/system/cpu/isolated
# expect: 8-287 (or whatever you set)

# (d) nohz_full took effect at runtime
cat /sys/devices/system/cpu/nohz_full
# expect: 8-287
# note: on some kernels the file prints "(null)" when no value
# was on the cmdline — that means nohz_full didn't take effect

# (e) cgroup v2 still active
stat -fc %T /sys/fs/cgroup
# expect: cgroup2fs
```

`/sys/devices/system/cpu/rcu_nocbs` is **not** present on every
kernel — `RCU_NOCB_CPU` only exposes that file when the cmdline
actually sets `rcu_nocbs=`. If `(b)` shows the arg on
`/proc/cmdline`, that's the authoritative check; `chaos-tuned
preflight` (used in step 7) cross-checks the same way.

Disable irqbalance permanently (the preflight check rejects a
profile with `globallyDisableIrqLoadBalancing: true` if irqbalance
is running):

```sh
sudo systemctl disable --now irqbalance
systemctl is-active irqbalance
# expect: inactive
```

---

## 6. Build the harness

```sh
git clone https://github.com/glennswest/chaos-harness.git
cd chaos-harness
make build
```

This produces five binaries in `bin/`:

```
bin/chaos-launcher
bin/chaos-worker
bin/chaos-victim
bin/chaos-observer
bin/chaos-tuned
```

Optional Go 1.25 binary for `run7-go125-no-cgroup`:

```sh
make build-go125
```

Run the unit tests:

```sh
make test
make vet
```

---

## 7. Edit the PerformanceProfile for your host

Open `examples/profiles/large-host-rt.yaml` and change three things:

1. `cpu.reserved` and `cpu.isolated` — must match the ranges you
   put on the kernel cmdline in step 4b.
2. `additionalKernelArgs:` — if you are on Intel, uncomment the
   `intel_idle.max_cstate=0` line.
3. `chaos-victim.cpus` — pick 4 cores at the top of the isolated
   range. See `docs/TUNING.md` for chiplet / SMT considerations.

Preflight the profile against the live host without running
anything:

```sh
./bin/chaos-tuned preflight --profile examples/profiles/large-host-rt.yaml
```

To see the full per-process plan (cpuset, GOMAXPROCS, RT priority
for every component the planner would spawn):

```sh
./bin/chaos-tuned explain --profile examples/profiles/large-host-rt.yaml \
                          --topology sno
```

Common failures and fixes:

| Failure | Fix |
|---|---|
| `realTimeKernel.enabled but uname -r missing rt` | Boot kernel-rt (step 3 → step 4 → reboot) |
| `isolcpus mismatch` | Cmdline doesn't match profile; update one or the other |
| `irqbalance is running` | `sudo systemctl disable --now irqbalance` |
| `cgroup v1 detected` | Switch to cgroup v2 (step 1) |

---

## 8. Smoke run (no tuning)

Verify the binaries work end-to-end on this host:

```sh
make build
./bin/chaos-launcher --config test-matrix/run-smoke-baseline.yaml \
                     --output-dir results/
```

Expected output:

```
results/run-smoke-baseline/
├── manifest.yaml
├── summary.json
├── summary.md
└── raw/
```

If `summary.md` is populated and `raw/worker-*.jsonl` files exist,
the harness is working. See `docs/OUTPUTS.md` for the format of
every file.

---

## 9. Tuned-RT run

Once preflight (step 7) passes and the smoke run (step 8) works:

```sh
sudo ./bin/chaos-launcher \
    --config test-matrix/run-tuned-rt.yaml \
    --output-dir results/
```

Why `sudo`: applying the PerformanceProfile requires writing to
systemd transient units and (on some backends) cgroup files, and
the RT priority on `chaos-victim` requires `CAP_SYS_NICE`. If you
do not have root, the launcher will fall back to `taskset` and skip
the RT priority — measurements will still produce output but the
victim's tail will not be RT-protected.

After the run, see `results/run-tuned-rt/summary.md` for the
headline numbers and `docs/OUTPUTS.md` for everything else.

---

## 10. Full matrix run

```sh
make matrix           # full duration, every canonical run
make matrix-quick     # 60s smoke version of every canonical run
```

To run a subset:

```sh
DURATION=60s ONLY="run1-baseline-64core run2-headline" \
    ./scripts/run-matrix.sh
```

---

## Privileges summary

| Run kind | Needs root? | Why |
|---|---|---|
| Smoke (no tuning) | no | cpuset/RT priority not enforced |
| Mitigation-only (`run4`, `run5`) | no | only changes `GOMAXPROCS` env |
| Tuned (`run8`, `run9`, `run-tuned-rt`) | **yes** | systemd-run / cgroup writes / RT priority |
| Matrix containing tuned runs | yes | as above |

If you need to run unprivileged, restrict yourself to the non-tuned
runs (`run1` through `run7`) — they exercise the failure mode
itself, which is the original purpose of the harness.

---

## Troubleshooting

* **`isolcpus=` ignored on AMD or older Intel parts** — some
  kernels require the `managed_irq,` prefix on the value. The
  cmdline above includes it. If `cat /sys/devices/system/cpu/isolated`
  comes back empty, drop the `managed_irq,` and try again.
* **Preflight rejects an obviously-correct profile** — run
  `chaos-tuned --verify` with `--debug` to see which probe is
  failing.
* **Worker counts in `summary.json` don't match the topology** —
  `observer.worker_pid_filter` matches process `comm` and
  `cmdline`. If you renamed the binary, update the filter.
* **`plot-*.png` missing after a run** — `matplotlib` is not
  installed; the rest of the output is fine. Install it with
  `sudo dnf install python3-matplotlib`.
