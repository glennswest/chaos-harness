// Package runconfig defines the launcher's run-config YAML schema.
package runconfig

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level run-config document.
type Config struct {
	RunID            string        `yaml:"run_id"`
	Duration         time.Duration `yaml:"duration"`
	Mode             string        `yaml:"mode"`             // drift | sync
	Topology         string        `yaml:"topology"`         // sno | master | worker | path/to/file.yaml
	WorkerGOMAXPROCS int           `yaml:"worker_gomaxprocs"` // 0 = inherit topology values

	// PerformanceProfile is the optional path to an OpenShift
	// PerformanceProfile YAML (with chaos-harness componentMap extension).
	// When set, the launcher resolves it against Topology to build a tuning
	// plan, preflights the host, applies cpuset/memory/RT limits to every
	// spawned process, and verifies each landed in its assigned cpuset.
	// Empty = no tuning (legacy behaviour).
	PerformanceProfile string `yaml:"performance_profile,omitempty"`

	// TuningBackend overrides the auto-selected applier. One of
	// "systemd-run", "cgroup-v2", "taskset". Empty = auto-select.
	TuningBackend string `yaml:"tuning_backend,omitempty"`

	SyncTrigger SyncTriggerConfig `yaml:"sync_trigger,omitempty"`
	Victim      VictimConfig      `yaml:"victim"`
	Observer    ObserverConfig    `yaml:"observer"`
}

// SyncTriggerConfig controls launcher-driven reconcile alignment in
// sync mode (§4.3).
type SyncTriggerConfig struct {
	InitialOffset time.Duration `yaml:"initial_offset"`
	Period        time.Duration `yaml:"period"`
}

// VictimConfig parameterises the chaos-victim child.
type VictimConfig struct {
	Mode        string `yaml:"mode"`         // hires_jitter | http_rtt
	PinningCPUs string `yaml:"pinning_cpus"` // empty = unconstrained
}

// ObserverConfig parameterises the chaos-observer child.
type ObserverConfig struct {
	SampleInterval  time.Duration `yaml:"sample_interval"`
	WorkerPIDFilter string        `yaml:"worker_pid_filter"`
}

// LoadFile reads, decodes, and applies defaults to a run-config YAML.
func LoadFile(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	return c, c.Validate()
}

func (c *Config) applyDefaults() {
	if c.Mode == "" {
		c.Mode = "drift"
	}
	if c.Duration <= 0 {
		c.Duration = 10 * time.Minute
	}
	if c.Topology == "" {
		c.Topology = "sno"
	}
	if c.Victim.Mode == "" {
		c.Victim.Mode = "hires_jitter"
	}
	if c.Observer.SampleInterval <= 0 {
		c.Observer.SampleInterval = time.Second
	}
	if c.Observer.WorkerPIDFilter == "" {
		c.Observer.WorkerPIDFilter = "chaos-worker"
	}
	if c.Mode == "sync" {
		if c.SyncTrigger.InitialOffset <= 0 {
			c.SyncTrigger.InitialOffset = 30 * time.Second
		}
		if c.SyncTrigger.Period <= 0 {
			c.SyncTrigger.Period = 15 * time.Second
		}
	}
}

// Validate returns the first invariant violation in c.
func (c Config) Validate() error {
	if c.RunID == "" {
		return fmt.Errorf("run_id is required")
	}
	if c.Mode != "drift" && c.Mode != "sync" {
		return fmt.Errorf("mode must be drift or sync, got %q", c.Mode)
	}
	if c.WorkerGOMAXPROCS < 0 {
		return fmt.Errorf("worker_gomaxprocs must be >= 0")
	}
	if c.Victim.Mode != "hires_jitter" && c.Victim.Mode != "http_rtt" {
		return fmt.Errorf("victim.mode must be hires_jitter or http_rtt, got %q", c.Victim.Mode)
	}
	switch c.TuningBackend {
	case "", "systemd-run", "cgroup-v2", "taskset":
	default:
		return fmt.Errorf("tuning_backend must be empty or one of systemd-run|cgroup-v2|taskset, got %q", c.TuningBackend)
	}
	return nil
}
