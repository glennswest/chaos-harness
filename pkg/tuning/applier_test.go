package tuning

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func sampleAssignment() Assignment {
	return Assignment{
		Component:  "kube-apiserver",
		ReplicaID:  "kube-apiserver-0",
		Class:      ClassReserved,
		CPUs:       MustParseCPUList("0-3"),
		Exclusive:  false,
		GOMAXPROCS: 4,
	}
}

func TestSystemdApplierWrap(t *testing.T) {
	a := sampleAssignment()
	a.MemoryMax = 1 << 30
	a.RTPriority = 50
	app := NewSystemdApplier()
	cmd := exec.CommandContext(context.Background(), "/usr/bin/chaos-worker", "--profile=control-plane", "--component=kube-apiserver")
	wrapped, err := app.Wrap(context.Background(), cmd, a)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	got := strings.Join(wrapped.Args, " ")
	for _, want := range []string{
		"systemd-run", "--scope", "--slice=chaos.slice",
		"--unit=chaos-kube-apiserver-kube-apiserver-0.scope",
		"--property=AllowedCPUs=0-3",
		"--property=MemoryMax=1073741824",
		"chrt -f 50",
		"/usr/bin/chaos-worker", "--profile=control-plane",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("wrapped args missing %q\ngot: %s", want, got)
		}
	}
	// GOMAXPROCS must be in env.
	envFound := false
	for _, e := range wrapped.Env {
		if e == "GOMAXPROCS=4" {
			envFound = true
		}
	}
	if !envFound {
		t.Errorf("GOMAXPROCS=4 not set in wrapped env: %v", wrapped.Env)
	}
}

func TestTasksetApplierWrap(t *testing.T) {
	a := sampleAssignment()
	a.NUMANode = -1
	a.RTPriority = 0
	app := NewTasksetApplier()
	cmd := exec.CommandContext(context.Background(), "/usr/bin/chaos-worker", "--profile=control-plane")
	wrapped, err := app.Wrap(context.Background(), cmd, a)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	got := strings.Join(wrapped.Args, " ")
	if !strings.Contains(got, "taskset -c 0-3") {
		t.Errorf("missing taskset -c 0-3: %s", got)
	}
	if !strings.Contains(got, "/usr/bin/chaos-worker") {
		t.Errorf("missing inner cmd: %s", got)
	}
}

func TestTasksetApplierMemoryRefused(t *testing.T) {
	a := sampleAssignment()
	a.MemoryMax = 1 << 30
	app := NewTasksetApplier()
	cmd := exec.CommandContext(context.Background(), "/usr/bin/chaos-worker")
	if _, err := app.Wrap(context.Background(), cmd, a); err == nil {
		t.Error("expected error: taskset cannot honor MemoryMax")
	}
}

func TestAppendOrReplaceEnv(t *testing.T) {
	env := []string{"PATH=/bin", "GOMAXPROCS=8", "USER=test"}
	got := appendOrReplaceEnv(env, "GOMAXPROCS", "4")
	want := []string{"PATH=/bin", "GOMAXPROCS=4", "USER=test"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
	got = appendOrReplaceEnv([]string{"PATH=/bin"}, "GOMAXPROCS", "4")
	want = []string{"PATH=/bin", "GOMAXPROCS=4"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSanitiseUnit(t *testing.T) {
	cases := map[string]string{
		"kube-apiserver":    "kube-apiserver",
		"kube_apiserver":    "kube_apiserver",
		"kube apiserver":    "kube_apiserver",
		"foo/bar":           "foo_bar",
		"prom@example.com":  "prom_example_com",
	}
	for in, want := range cases {
		if got := sanitiseUnit(in); got != want {
			t.Errorf("sanitiseUnit(%q) = %q, want %q", in, got, want)
		}
	}
}
