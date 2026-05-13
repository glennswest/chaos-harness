package tuning

import "testing"

func TestExtractKernelArg(t *testing.T) {
	cmdline := "BOOT_IMAGE=/vmlinuz root=UUID=abc isolcpus=managed_irq,4-31 nohz_full=4-31 rcu_nocbs=4-31 default_hugepagesz=1G hugepagesz=1G hugepages=16"
	cases := map[string]string{
		"isolcpus":           "managed_irq,4-31",
		"nohz_full":          "4-31",
		"rcu_nocbs":          "4-31",
		"default_hugepagesz": "1G",
		"hugepagesz":         "1G",
		"hugepages":          "16",
		"missing":            "",
	}
	for k, want := range cases {
		if got := extractKernelArg(cmdline, k); got != want {
			t.Errorf("extractKernelArg(%q) = %q, want %q", k, got, want)
		}
	}
}

func TestStripManagedSuffix(t *testing.T) {
	cases := map[string]string{
		"managed_irq,4-31": "4-31",
		"4-31":             "4-31",
		"domain,4-31":      "4-31",
	}
	for in, want := range cases {
		if got := stripManagedSuffix(in); got != want {
			t.Errorf("stripManagedSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVerifyPIDAcceptsSubset(t *testing.T) {
	// Synthesise a /proc/X/status content and exercise the parser path
	// directly via ParsePIDStatusForCPUs. Full VerifyPID needs a real PID.
	status := `Name:	chaos-worker
State:	S (sleeping)
Cpus_allowed:	ff
Cpus_allowed_list:	4-7
`
	got, err := ParsePIDStatusForCPUs(status)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.String() != "4-7" {
		t.Errorf("got %q want 4-7", got)
	}
}
