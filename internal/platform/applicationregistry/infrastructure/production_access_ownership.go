package infrastructure

import (
	"os"
	"syscall"
)

// inheritAccessProofOwner 让受控证明继承运行目录属主，不放宽私有读写权限。
// 参数：file为已验证打开的证明文件或目录，runtime为既有部署运行目录。
// 返回值：属主已一致返回nil；目录不安全或无法交接属主时失败关闭。
func inheritAccessProofOwner(file *os.File, runtime string) error {
	owner, err := os.Lstat(runtime)
	if err != nil {
		return err
	}
	if !owner.IsDir() || owner.Mode()&os.ModeSymlink != 0 {
		return provisioningError("deployment proof owner directory is unsafe")
	}
	wanted, ok := owner.Sys().(*syscall.Stat_t)
	if !ok {
		return provisioningError("deployment proof owner is unavailable")
	}
	current, err := file.Stat()
	if err != nil {
		return err
	}
	actual, ok := current.Sys().(*syscall.Stat_t)
	if !ok {
		return provisioningError("deployment proof file owner is unavailable")
	}
	if actual.Uid == wanted.Uid && actual.Gid == wanted.Gid {
		return nil
	}
	return file.Chown(int(wanted.Uid), int(wanted.Gid))
}

// inheritAccessDirectoryOwner 仅交接已核验私有结果目录的属主。
// 参数：path为受控结果目录，runtime为备份用户拥有的既有运行目录。
// 返回值：无法打开或安全继承属主时返回错误，不执行宽松chmod。
func inheritAccessDirectoryOwner(path, runtime string) error {
	directory, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer directory.Close()
	return inheritAccessProofOwner(directory, runtime)
}
