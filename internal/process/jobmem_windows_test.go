//go:build windows

package process

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobMemoryLimit reads back the memory limit configured on a tree's job.
func jobMemoryLimit(t *tree) (uint64, error) {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	err := windows.QueryInformationJobObject(t.job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
	return uint64(info.JobMemoryLimit), err
}
