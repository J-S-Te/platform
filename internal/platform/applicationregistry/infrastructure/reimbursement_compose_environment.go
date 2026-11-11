package infrastructure

import (
	"bufio"
	"errors"
	"os"
	"strings"
	"syscall"
)

// reimbursementComposeEnvironment 将完整报销配置限定为服务级注入，仅提取两个专属数据库插值。
// 参数：path 为受控运行文件，required 表示本次是否实际选择报销组件。
// 返回值：服务文件路径和专属插值；缺失必需配置或不安全文件时返回不含配置内容的错误。
func reimbursementComposeEnvironment(path string, required bool) (string, []string, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) && !required {
		return os.DevNull, nil, nil
	}
	if err != nil {
		return "", nil, provisioningError("reimbursement runtime environment is unavailable or unsafe")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 1<<20 {
		return "", nil, provisioningError("reimbursement runtime environment must be a private regular file")
	}
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(scanner.Text()), "export "))
		key, value, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if found && (key == "REIMBURSEMENT_MYSQL_PASSWORD" || key == "REIMBURSEMENT_MYSQL_ROOT_PASSWORD") {
			values[key] = strings.Trim(strings.TrimSpace(value), "\"'")
		}
	}
	if scanner.Err() != nil {
		return "", nil, provisioningError("reimbursement runtime environment cannot be parsed")
	}
	interpolation := []string{}
	for _, key := range []string{"REIMBURSEMENT_MYSQL_PASSWORD", "REIMBURSEMENT_MYSQL_ROOT_PASSWORD"} {
		if values[key] != "" {
			interpolation = append(interpolation, key+"="+values[key])
		}
	}
	return path, interpolation, nil
}
