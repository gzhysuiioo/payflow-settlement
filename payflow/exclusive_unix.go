//go:build unix

package payflow

import (
	"errors"
	"os"
	"syscall"
)

// openExclusiveCreate 以互斥方式创建 path：
//   - O_CREATE|O_EXCL：文件已存在（含另一进程刚刚抢先创建完成的账本）时
//     失败并返回 EEXIST，内核保证同一目标上相互重叠的创建请求只有一个成功；
//   - O_NOFOLLOW：path 本身是符号链接时一律失败（即使链接悬空、目标不存在，
//     内核以 ELOOP 拒绝），既不会替换链接，也不会顺着链接创建目标文件；
//   - O_WRONLY：仅用于写入一份完整文档，不读取、不截断任何已有内容。
//
// 路径中间的目录符号链接仍正常跟随——经目录软链接在尚未占用的文件位置
// 创建账本是允许的。
func openExclusiveCreate(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
}

// isOccupiedCreateErr 报告创建失败是否因为路径已被占用：已有同名文件/目录
// （EEXIST/EISDIR），或文件位置上存在符号链接（O_NOFOLLOW 触发 ELOOP，
// 含链接目标不存在的悬空链接）。
func isOccupiedCreateErr(err error) bool {
	return errors.Is(err, syscall.EEXIST) ||
		errors.Is(err, syscall.ELOOP) ||
		errors.Is(err, syscall.EISDIR)
}
