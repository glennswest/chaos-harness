module github.com/glennswest/chaos-harness

go 1.24

// Dependencies are added as features land:
//   github.com/HdrHistogram/hdrhistogram-go  (chaos-victim, day 10)
//   golang.org/x/sys                         (sched_setaffinity, day 10)
//   gopkg.in/yaml.v3                         (chaos-launcher, day 8)

require (
	github.com/HdrHistogram/hdrhistogram-go v1.2.0 // indirect
	golang.org/x/sys v0.27.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
