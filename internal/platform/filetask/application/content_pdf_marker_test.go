package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// TestPDFMarkerStaticContent 验证合法注释、文字及RGB像素不被原始标记误拒。
// 参数：t为只调用存储内容校验器的测试上下文。
// 返回值：无；静态原件被误拒或字节改变时失败。
func TestPDFMarkerStaticContent(t *testing.T) {
	for _, kind := range []string{"comment", "text", "RGB"} {
		t.Run(kind, func(t *testing.T) {
			body := pdfMarkerFixture(kind)
			before := sha256.Sum256(body)
			if err := validateStoredContent("application/pdf", bytes.NewReader(body)); err != nil {
				t.Fatalf("合法静态%s被拒绝: %v", kind, err)
			}
			if before != sha256.Sum256(body) {
				t.Fatal("文件网关校验改写了原件")
			}
		})
	}
}

// TestPDFMarkerCompatibility 验证无标记PDF信封兼容及真实活动对象拒绝。
// 参数：t为网关格式准入测试上下文。
// 返回值：无；原信封行为改变或活动内容被接受时失败。
func TestPDFMarkerCompatibility(t *testing.T) {
	if err := validateStoredContent("application/pdf",
		bytes.NewReader([]byte("%PDF-1.7\n%%EOF\n"))); err != nil {
		t.Fatalf("无标记旧信封行为改变: %v", err)
	}
	if err := validateStoredContent("application/pdf",
		bytes.NewReader(pdfMarkerFixture("action"))); err == nil {
		t.Fatal("真实JavaScript动作被接受")
	}
}

// TestPDFMarkerEncodedActions 验证升级分支仍拒绝名称转义和压缩对象中的活动内容。
// 参数：t为真实对象解析测试上下文。
// 返回值：无；任一编码动作通过存储校验时失败。
func TestPDFMarkerEncodedActions(t *testing.T) {
	encoded := pdfMarkerFixture("encoded")
	configuration := model.NewStatelessConfiguration()
	configuration.Offline = true
	configuration.WriteObjectStream, configuration.WriteXRefStream = true, true
	var compressed bytes.Buffer
	if err := api.Optimize(context.Background(), bytes.NewReader(encoded),
		&compressed, configuration, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(compressed.Bytes(), []byte("/ObjStm")) {
		t.Fatal("夹具没有产生压缩对象流")
	}
	// 合法尾部注释触发旧标记分支，禁止动作本身仍藏于压缩对象中。
	compressed.WriteString("\n% /JS static marker\n")
	for _, body := range [][]byte{encoded, compressed.Bytes()} {
		if err := validateStoredContent("application/pdf", bytes.NewReader(body)); err == nil {
			t.Fatal("编码或压缩的禁止动作被接受")
		}
	}
}

// pdfMarkerFixture 生成具有精确交叉引用的静态或负向动作原件。
// 参数：kind指定合法注释、文本、原始RGB像素或禁止动作。
// 返回值：可离线解析的单页PDF；不渲染或上传任何样本。
func pdfMarkerFixture(kind string) []byte {
	content := "BT /F1 12 Tf 10 100 Td (static /JS text) Tj ET"
	resource := "/Font << /F1 4 0 R >>"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>", "",
	}
	comment := ""
	if kind == "comment" {
		comment, content = "% literal /JS in comment\n", ""
	}
	if kind == "RGB" {
		content, resource = "q 100 0 0 100 0 0 cm /Scan Do Q", "/XObject << /Scan 4 0 R >>"
		objects[3] = "<< /Type /XObject /Subtype /Image /Width 1 /Height 1 " +
			"/ColorSpace /DeviceRGB /BitsPerComponent 8 /Length 3 >>\nstream\n/JS\nendstream"
	}
	if kind == "action" || kind == "encoded" {
		objects[0] = "<< /Type /Catalog /Pages 2 0 R /OpenAction 6 0 R >>"
		objects = append(objects, "<< /S /JavaScript /JS (app.alert\\(1\\)) >>")
		if kind == "encoded" {
			objects[5] = "<< /S /Java#53cript /J#53 (app.alert\\(1\\)) >>"
		}
	}
	objects[2] = "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] " +
		"/Resources << " + resource + " >> /Contents 5 0 R >>"
	objects[4] = fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content)
	var output strings.Builder
	output.WriteString("%PDF-1.7\n" + comment)
	offsets := []int{0}
	for index, object := range objects {
		offsets = append(offsets, output.Len())
		fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	start := output.Len()
	fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF",
		len(objects)+1, start)
	return []byte(output.String())
}
