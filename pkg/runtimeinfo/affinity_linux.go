//go:build linux

package runtimeinfo

import (
	"syscall"
	"unsafe"
)

// SchedGetaffinity returns the calling thread's CPU set.
//
// Implemented via the raw sched_getaffinity syscall (number 204 on
// amd64, 122 on arm64; see affinity_linux_<arch>.go). We use a
// 1024-CPU mask which covers any reasonable bare-metal node and a
// comfortable margin.
const cpuSetBytes = 128 // 1024 bits

func readAffinity() string {
	var mask [cpuSetBytes]byte
	r1, _, errno := syscall.Syscall(sysSchedGetaffinityNum, 0, uintptr(cpuSetBytes), uintptr(unsafe.Pointer(&mask[0])))
	if errno != 0 || int(r1) < 0 {
		return ""
	}
	cpus := make([]int, 0, 16)
	for byteIdx := 0; byteIdx < cpuSetBytes; byteIdx++ {
		b := mask[byteIdx]
		if b == 0 {
			continue
		}
		for bit := 0; bit < 8; bit++ {
			if b&(1<<bit) != 0 {
				cpus = append(cpus, byteIdx*8+bit)
			}
		}
	}
	if len(cpus) == 0 {
		return ""
	}
	return FormatCPUSet(cpus)
}
