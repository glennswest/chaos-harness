#!/usr/bin/env python3
"""Aggregate one chaos-harness run into summary.json + summary.md.

Inputs (under --run-dir):
    raw/worker-*.jsonl      — per-worker event streams
    raw/observer.csv        — system /proc time-series
    raw/victim.hdr          — HDR snapshot (value_ns, count)
    raw/victim-buckets.csv  — per-second jitter percentiles
    manifest.yaml           — resolved run metadata

Outputs (in --run-dir):
    summary.json
    summary.md

Headline metrics (per design §7.2):
    peak_total_threads
    mean_total_threads
    peak_aggregate_rss_mb
    alignment_events_per_min   (1s windows where >=3 workers in GC)
    victim_p99_us
    victim_p999_us
    victim_p99_baseline_multiplier (only if --baseline supplied)
    process_count
    profile_breakdown

Designed to run with the Python stdlib only — pandas/matplotlib are
optional and only used when present (for the plotting pass). Without
them the tool still produces summary.json and summary.md, just no PNGs.
"""

from __future__ import annotations

import argparse
import csv
import glob
import json
import os
import statistics
import sys
from collections import Counter, defaultdict
from pathlib import Path

try:
    import yaml  # type: ignore
except ImportError:
    yaml = None


def load_manifest(run_dir: Path):
    p = run_dir / "manifest.yaml"
    if not p.exists():
        return None
    if yaml is None:
        return {"_note": "manifest.yaml present but PyYAML missing"}
    with p.open() as f:
        return yaml.safe_load(f)


def iter_worker_events(raw_dir: Path):
    """Yield (worker_path, event_dict) for every JSONL event."""
    for path in sorted(raw_dir.glob("worker-*.jsonl")):
        with path.open() as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                try:
                    yield path, json.loads(line)
                except json.JSONDecodeError:
                    continue


def aggregate_workers(raw_dir: Path):
    """Return per-worker summary dicts and time-bucketed gc activity."""
    per_worker = {}
    gc_events_by_second = defaultdict(set)  # second_ts -> set of workers active in GC
    reconcile_events = defaultdict(int)
    for path, ev in iter_worker_events(raw_dir):
        worker_key = path.stem
        wd = per_worker.setdefault(worker_key, {
            "worker": worker_key,
            "samples": [],
            "gc_events": 0,
            "reconcile_events": 0,
            "startup": None,
            "shutdown": None,
            "max_threads": 0,
            "max_goroutines": 0,
            "max_rss_bytes": 0,
        })
        t = ev.get("type")
        if t == "startup":
            wd["startup"] = ev
        elif t == "shutdown":
            wd["shutdown"] = ev
        elif t == "sample":
            wd["samples"].append(ev)
            wd["max_threads"] = max(wd["max_threads"], ev.get("threads", 0))
            wd["max_goroutines"] = max(wd["max_goroutines"], ev.get("goroutines", 0))
            wd["max_rss_bytes"] = max(wd["max_rss_bytes"], ev.get("rss_bytes", 0))
        elif t == "gc_event":
            wd["gc_events"] += 1
            sec = int(ev.get("ts", 0))
            gc_events_by_second[sec].add(worker_key)
        elif t == "reconcile_start":
            wd["reconcile_events"] += 1
            reconcile_events[int(ev.get("ts", 0))] += 1
    return per_worker, gc_events_by_second, reconcile_events


def alignment_stats(gc_events_by_second, threshold=3):
    """Count seconds where >= threshold workers were active in GC."""
    seconds = sorted(gc_events_by_second.keys())
    aligned = sum(1 for s in seconds if len(gc_events_by_second[s]) >= threshold)
    span_min = max((max(seconds) - min(seconds)) / 60.0, 1/60) if seconds else 1
    rate_per_min = aligned / span_min if span_min else 0
    return aligned, rate_per_min


def parse_observer_csv(path: Path):
    rows = []
    if not path.exists():
        return rows
    with path.open() as f:
        for row in csv.DictReader(f):
            rows.append(row)
    return rows


def observer_peak(rows, key, conv=int):
    if not rows:
        return 0
    return max(conv(r[key]) for r in rows if r.get(key))


def observer_mean(rows, key, conv=float):
    if not rows:
        return 0
    vals = [conv(r[key]) for r in rows if r.get(key)]
    if not vals:
        return 0
    return sum(vals) / len(vals)


def parse_victim_buckets(path: Path):
    if not path.exists():
        return None
    last = None
    p99s = []
    p999s = []
    with path.open() as f:
        for row in csv.DictReader(f):
            try:
                p99 = int(row.get("p99_us", "0"))
                p999 = int(row.get("p999_us", "0"))
            except ValueError:
                continue
            p99s.append(p99)
            p999s.append(p999)
            last = row
    if not p99s:
        return None
    return {
        "p99_us": int(statistics.median(p99s)),
        "p999_us": int(statistics.median(p999s)),
        "p99_us_max": max(p99s),
        "p999_us_max": max(p999s),
        "samples": len(p99s),
        "last_row": last,
    }


def build_summary(run_dir: Path, baseline_p99_us=None):
    raw_dir = run_dir / "raw"
    manifest = load_manifest(run_dir)
    per_worker, gc_by_sec, reconcile_by_sec = aggregate_workers(raw_dir)
    obs_rows = parse_observer_csv(raw_dir / "observer.csv")
    victim = parse_victim_buckets(raw_dir / "victim-buckets.csv")
    aligned_count, aligned_per_min = alignment_stats(gc_by_sec)

    profile_breakdown = Counter()
    component_breakdown = Counter()
    for w in per_worker.values():
        s = w.get("startup") or {}
        if s.get("profile"):
            profile_breakdown[s["profile"]] += 1
        if s.get("component"):
            component_breakdown[s["component"]] += 1

    headline = {
        "process_count": len(per_worker),
        "peak_total_threads": observer_peak(obs_rows, "total_threads"),
        "mean_total_threads": int(observer_mean(obs_rows, "total_threads")),
        "peak_aggregate_rss_mb": int(observer_peak(obs_rows, "total_rss_mb", conv=float)),
        "peak_loadavg_1": observer_peak(obs_rows, "loadavg_1", conv=float),
        "peak_ctxt_per_sec": observer_peak(obs_rows, "ctxt_per_sec"),
        "alignment_events_aligned_seconds": aligned_count,
        "alignment_events_per_min": round(aligned_per_min, 2),
    }
    if victim:
        headline["victim_p99_us"] = victim["p99_us"]
        headline["victim_p999_us"] = victim["p999_us"]
        headline["victim_p99_us_max"] = victim["p99_us_max"]
        headline["victim_p999_us_max"] = victim["p999_us_max"]
        if baseline_p99_us:
            headline["victim_p99_baseline_multiplier"] = round(
                victim["p99_us"] / max(baseline_p99_us, 1), 1
            )

    summary = {
        "run_id": (manifest or {}).get("run_id") or run_dir.name,
        "manifest": manifest,
        "headline": headline,
        "profile_breakdown": dict(profile_breakdown),
        "component_breakdown": dict(component_breakdown),
        "per_worker": [
            {
                "worker": w["worker"],
                "profile": (w.get("startup") or {}).get("profile"),
                "component": (w.get("startup") or {}).get("component"),
                "gomaxprocs": (w.get("startup") or {}).get("gomaxprocs"),
                "max_threads": w["max_threads"],
                "max_goroutines": w["max_goroutines"],
                "max_rss_mb": round(w["max_rss_bytes"] / 1024 / 1024, 1),
                "gc_events": w["gc_events"],
                "reconcile_events": w["reconcile_events"],
            }
            for w in sorted(per_worker.values(), key=lambda x: x["worker"])
        ],
    }
    return summary


def write_md(run_dir: Path, summary: dict):
    h = summary["headline"]
    pb = summary["profile_breakdown"]
    md = []
    md.append(f"# Chaos Harness Run — `{summary['run_id']}`\n")
    if summary.get("manifest"):
        m = summary["manifest"]
        md.append(f"- **Topology:** `{m.get('topology_name')}` "
                  f"(host_type=`{m.get('topology_host_type')}`)\n")
        md.append(f"- **Duration:** {m.get('duration')}  ")
        md.append(f"**Mode:** {m.get('mode')}  ")
        md.append(f"**Worker GOMAXPROCS override:** {m.get('worker_gomaxprocs')}\n")
        md.append(f"- **Started:** {m.get('started_at')}\n")
    md.append("\n## Headline\n\n")
    md.append("| Metric | Value |\n|---|---|\n")
    md.append(f"| Process count | {h.get('process_count')} |\n")
    md.append(f"| Peak total worker threads | **{h.get('peak_total_threads')}** |\n")
    md.append(f"| Mean total worker threads | {h.get('mean_total_threads')} |\n")
    md.append(f"| Peak aggregate RSS (MB) | {h.get('peak_aggregate_rss_mb')} |\n")
    md.append(f"| Peak loadavg(1m) | {h.get('peak_loadavg_1')} |\n")
    md.append(f"| Peak ctxt switches/sec | {h.get('peak_ctxt_per_sec')} |\n")
    md.append(f"| GC alignment events (≥3 workers/sec) | {h.get('alignment_events_aligned_seconds')} |\n")
    md.append(f"| GC alignment events / min | {h.get('alignment_events_per_min')} |\n")
    if "victim_p99_us" in h:
        md.append(f"| Victim p99 jitter (µs, median over run) | {h.get('victim_p99_us')} |\n")
        md.append(f"| Victim p99.9 jitter (µs, median over run) | {h.get('victim_p999_us')} |\n")
        md.append(f"| Victim p99 jitter peak (µs) | {h.get('victim_p99_us_max')} |\n")
        md.append(f"| Victim p99.9 jitter peak (µs) | {h.get('victim_p999_us_max')} |\n")
        if "victim_p99_baseline_multiplier" in h:
            md.append(f"| Victim p99 baseline multiplier | **×{h.get('victim_p99_baseline_multiplier')}** |\n")

    md.append("\n## Profile breakdown\n\n")
    md.append("| Profile | Process count |\n|---|---|\n")
    for k in sorted(pb):
        md.append(f"| {k} | {pb[k]} |\n")

    md.append("\n## Per-worker summary (top 30 by max_threads)\n\n")
    md.append("| Worker | Component | Profile | GMP | max_threads | max_goroutines | max_rss_mb | gc | reconcile |\n")
    md.append("|---|---|---|---|---|---|---|---|---|\n")
    for w in sorted(summary["per_worker"], key=lambda x: x["max_threads"], reverse=True)[:30]:
        md.append(
            f"| {w['worker']} | {w.get('component') or '-'} | {w['profile']} | {w['gomaxprocs']} | "
            f"{w['max_threads']} | {w['max_goroutines']} | {w['max_rss_mb']} | {w['gc_events']} | {w['reconcile_events']} |\n"
        )

    out = run_dir / "summary.md"
    out.write_text("".join(md))
    return out


def maybe_plot(run_dir: Path):
    """Optional matplotlib plots — total threads, victim p99, alignment.

    Skipped silently if matplotlib is not installed.
    """
    try:
        import matplotlib  # type: ignore
        matplotlib.use("Agg")
        import matplotlib.pyplot as plt  # type: ignore
    except ImportError:
        return []
    raw = run_dir / "raw"
    plots = []

    obs_path = raw / "observer.csv"
    if obs_path.exists():
        ts, threads = [], []
        with obs_path.open() as f:
            for r in csv.DictReader(f):
                try:
                    ts.append(float(r["ts"]))
                    threads.append(int(r["total_threads"]))
                except (KeyError, ValueError):
                    continue
        if ts:
            t0 = ts[0]
            fig, ax = plt.subplots(figsize=(10, 4))
            ax.plot([t - t0 for t in ts], threads)
            ax.set_xlabel("seconds since start")
            ax.set_ylabel("total worker OS threads")
            ax.set_title(f"{run_dir.name}: total worker threads vs time")
            ax.grid(True, alpha=0.3)
            p = run_dir / "plot-threads.png"
            fig.tight_layout()
            fig.savefig(p, dpi=120)
            plt.close(fig)
            plots.append(p)

    vic_path = raw / "victim-buckets.csv"
    if vic_path.exists():
        ts, p99, p999 = [], [], []
        with vic_path.open() as f:
            for r in csv.DictReader(f):
                try:
                    ts.append(float(r["ts"]))
                    p99.append(int(r["p99_us"]))
                    p999.append(int(r["p999_us"]))
                except (KeyError, ValueError):
                    continue
        if ts:
            t0 = ts[0]
            fig, ax = plt.subplots(figsize=(10, 4))
            ax.plot([t - t0 for t in ts], p99, label="p99")
            ax.plot([t - t0 for t in ts], p999, label="p99.9")
            ax.set_xlabel("seconds since start")
            ax.set_ylabel("victim wakeup jitter (µs)")
            ax.set_title(f"{run_dir.name}: victim jitter vs time")
            ax.set_yscale("log")
            ax.grid(True, alpha=0.3, which="both")
            ax.legend()
            p = run_dir / "plot-victim.png"
            fig.tight_layout()
            fig.savefig(p, dpi=120)
            plt.close(fig)
            plots.append(p)
    return plots


def main(argv=None):
    ap = argparse.ArgumentParser()
    ap.add_argument("--run-dir", required=True)
    ap.add_argument("--config", help="run-config YAML (informational)")
    ap.add_argument("--baseline-p99-us", type=int, default=None,
                    help="baseline run's victim p99 (µs) for multiplier computation")
    args = ap.parse_args(argv)

    run_dir = Path(args.run_dir)
    if not run_dir.is_dir():
        print(f"aggregate: not a directory: {run_dir}", file=sys.stderr)
        return 1

    summary = build_summary(run_dir, baseline_p99_us=args.baseline_p99_us)

    json_path = run_dir / "summary.json"
    json_path.write_text(json.dumps(summary, indent=2, default=str))
    md_path = write_md(run_dir, summary)
    plots = maybe_plot(run_dir)

    print(f"aggregate: wrote {json_path}")
    print(f"aggregate: wrote {md_path}")
    for p in plots:
        print(f"aggregate: wrote {p}")
    h = summary["headline"]
    print("--- headline ---")
    for k in sorted(h):
        print(f"  {k} = {h[k]}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
