//go:build portal_report_mysql

package filetaskhttp

import (
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	mysqlconfig "github.com/go-sql-driver/mysql"
)

// portalGatewayOwnedDSN 读取显式独占测试配置，新增入口只允许本轮随机回环空库。
// 参数：t为测试上下文；新入口是FILE_GATEWAY_LOCAL_TEST_DSN，旧入口保持原固定schema文件。
// 返回值：专用DSN及是否必须核验空库；不符合边界时在连接前失败。
func portalGatewayOwnedDSN(t *testing.T) (string, bool) {
	t.Helper()
	if dsn := os.Getenv("FILE_GATEWAY_LOCAL_TEST_DSN"); dsn != "" {
		cfg, err := mysqlconfig.ParseDSN(dsn)
		if err != nil {
			t.Fatal("invalid disposable gateway test DSN")
		}
		host, rawPort, err := net.SplitHostPort(cfg.Addr)
		port, portErr := strconv.Atoi(rawPort)
		if err != nil || portErr != nil || host != "127.0.0.1" || port < 32768 || cfg.Net != "tcp" ||
			!regexp.MustCompile(`^file_gateway_local_test_[a-f0-9]{8}$`).MatchString(cfg.DBName) {
			t.Fatal("requires uniquely named disposable gateway database on loopback high port")
		}
		return dsn, true
	}
	path := os.Getenv("UIP_P1_FILE_GATEWAY_TEST_DSN_FILE")
	if path == "" {
		t.Fatal("owned MySQL DSN file or disposable gateway DSN is required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read owned MySQL DSN file failed")
	}
	dsn := strings.TrimSpace(string(raw))
	parsed, err := mysqlconfig.ParseDSN(dsn)
	if err != nil || parsed.DBName != "file_gateway_p1_test_20261013" {
		t.Fatal("refuse non-owned MySQL schema")
	}
	return dsn, false
}
