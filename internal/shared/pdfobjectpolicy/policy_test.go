package pdfobjectpolicy

import (
	"fmt"
	"strings"
	"testing"
)

// TestPolicyForbiddenObjects 验证所有既有禁止名称在真实对象中仍失败关闭。
// 参数：t为只做离线解析的测试上下文。
// 返回值：无；任一禁止动作、嵌入或加密名称通过时失败。
func TestPolicyForbiddenObjects(t *testing.T) {
	for _, name := range []string{"JavaScript", "JS", "EmbeddedFile", "EmbeddedFiles", "Launch", "Encrypt"} {
		body := admissionPDF([]string{
			"<< /Type /Catalog /Pages 2 0 R /OpenAction 4 0 R >>",
			"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 10 10] /Resources << >> >>",
			fmt.Sprintf("<< /S /%s >>", name),
		}, "")
		if err := validatePolicyFixture(body); err == nil {
			t.Fatalf("禁止对象%s通过", name)
		}
	}
}

// TestPolicyCallerSizeLimit 验证解析器采用调用方上限而非固定合同20MiB。
// 参数：t为构造21MiB合法静态PDF的本地测试上下文。
// 返回值：无；实际已准入上限误拒或小于输入的上限未拒绝时失败。
func TestPolicyCallerSizeLimit(t *testing.T) {
	body := admissionPDF([]string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 10 10] /Resources << >> >>",
	}, "% /JS "+strings.Repeat("P", 21<<20)+"\n")
	if err := Validate(body, int64(len(body))); err != nil {
		t.Fatalf("调用方已准入的大文件被误拒: %v", err)
	}
	if err := Validate(body, int64(len(body)-1)); err == nil {
		t.Fatal("输入超出调用方上限仍被接受")
	}
}
