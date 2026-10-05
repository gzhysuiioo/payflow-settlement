//go:build !unix

package payflow

import (
	"os"
	"syscall"
)

// openExclusiveCreate 是非 Unix 平台的兜底实现：O_CREATE|O_EXCL 由内核保证
// 同一目标上相互重叠的创建只有一个成功（文件已存在即失败）。
// 平台若不提供 O_NOFOLLOW 等价物，则在创建前对路径本身显式 lstat：
// 文件位置上存在符号链接（含悬空链接）时直接按占用拒绝，既不替换链接，
// 也不顺着链接创建目标文件。中间路径的目录符号链接不受影响。
func openExclusiveCreate(path string) (*os.File, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EEXIST}
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

// isOccupiedCreateErr 报告创建失败是否因为路径已被占用。
func isOccupiedCreateErr(err error) bool {
	return os.IsExist(err)
}
