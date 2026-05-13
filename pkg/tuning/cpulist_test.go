package tuning

import (
	"testing"
)

func TestParseCPUList(t *testing.T) {
	cases := []struct {
		in      string
		want    string // expected canonical form
		wantErr bool
	}{
		{"", "", false},
		{"  ", "", false},
		{"0", "0", false},
		{"0-3", "0-3", false},
		{"0-3,8,12-15", "0-3,8,12-15", false},
		{"15,12,13,14,0,1,2,3,8", "0-3,8,12-15", false}, // sort + dedupe
		{"0,0,0", "0", false},                            // dedupe
		{"0-1,1-2", "0-2", false},                        // overlap
		{"5-3", "", true},                                // reverse
		{"abc", "", true},
		{"-1", "", true},
		{"0-", "", true},
	}
	for _, tc := range cases {
		got, err := ParseCPUList(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseCPUList(%q) want err, got %q", tc.in, got.String())
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseCPUList(%q) unexpected err: %v", tc.in, err)
			continue
		}
		if got.String() != tc.want {
			t.Errorf("ParseCPUList(%q) = %q, want %q", tc.in, got.String(), tc.want)
		}
	}
}

func TestCPUListSetOps(t *testing.T) {
	a := MustParseCPUList("0-3,8")
	b := MustParseCPUList("2-5,8")
	if got := a.Intersect(b).String(); got != "2-3,8" {
		t.Errorf("intersect = %q, want %q", got, "2-3,8")
	}
	if got := a.Difference(b).String(); got != "0-1" {
		t.Errorf("difference = %q, want %q", got, "0-1")
	}
	if got := a.Union(b).String(); got != "0-5,8" {
		t.Errorf("union = %q, want %q", got, "0-5,8")
	}
	if !a.Disjoint(MustParseCPUList("10-20")) {
		t.Error("0-3,8 should be disjoint from 10-20")
	}
	if a.Disjoint(b) {
		t.Error("a and b should not be disjoint")
	}
}

func TestCPUListTake(t *testing.T) {
	all := MustParseCPUList("4-15")
	taken, rest := all.Take(4)
	if got := taken.String(); got != "4-7" {
		t.Errorf("take = %q, want %q", got, "4-7")
	}
	if got := rest.String(); got != "8-15" {
		t.Errorf("rest = %q, want %q", got, "8-15")
	}

	// take more than available
	taken, rest = all.Take(100)
	if got := taken.String(); got != "4-15" {
		t.Errorf("take all = %q, want %q", got, "4-15")
	}
	if rest.Len() != 0 {
		t.Errorf("rest after taking all = %q, want empty", rest.String())
	}

	// take zero
	taken, rest = all.Take(0)
	if taken.Len() != 0 {
		t.Errorf("take 0 = %q, want empty", taken.String())
	}
	if rest.String() != "4-15" {
		t.Errorf("rest after take 0 = %q, want 4-15", rest.String())
	}
}
