//go:build windows

package processtree

import "unsafe"

const (
	wantSchedulingClassOffset          = 20 + 5*unsafe.Sizeof(uintptr(0))
	wantIOCountersOffset               = 32 + 4*unsafe.Sizeof(uintptr(0))
	wantJobObjectExtendedLimitInfoSize = 80 + 8*unsafe.Sizeof(uintptr(0))
)

var (
	_ [wantSchedulingClassOffset - unsafe.Offsetof(jobObjectBasicLimitInformation{}.schedulingClass)]byte
	_ [unsafe.Offsetof(jobObjectBasicLimitInformation{}.schedulingClass) - wantSchedulingClassOffset]byte
	_ [wantIOCountersOffset - unsafe.Offsetof(jobObjectExtendedLimitInfo{}.ioInfo)]byte
	_ [unsafe.Offsetof(jobObjectExtendedLimitInfo{}.ioInfo) - wantIOCountersOffset]byte
	_ [wantJobObjectExtendedLimitInfoSize - unsafe.Sizeof(jobObjectExtendedLimitInfo{})]byte
	_ [unsafe.Sizeof(jobObjectExtendedLimitInfo{}) - wantJobObjectExtendedLimitInfoSize]byte
)
