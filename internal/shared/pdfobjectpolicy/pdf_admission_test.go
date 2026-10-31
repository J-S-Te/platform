package pdfobjectpolicy

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

// TestExternalPDFStaticActionWords 验证静态注释及文字不会被误判为活动对象。
// 参数：t为真实PDF准入测试上下文。
// 返回值：无；静态内容被拒绝或原件哈希变化时失败。
func TestExternalPDFStaticActionWords(t *testing.T) {
	content := "BT /F1 12 Tf 10 100 Td (/JS /JavaScript /EmbeddedFile /Launch /Encrypt) Tj ET"
	body := admissionPDF([]string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] " +
			"/Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
	}, "% static /JS /JavaScript /EmbeddedFile /Launch /Encrypt\n")
	before := sha256.Sum256(body)
	err := validatePolicyFixture(body)
	if err != nil {
		t.Fatalf("合法静态内容被拒绝: %v", err)
	}
	if before != sha256.Sum256(body) {
		t.Fatal("静态原件被改写或虚构提取正文")
	}
}

// TestExternalPDFImageBudgets 验证无文字扫描件及图片像素和渲染预算。
// 参数：t为只做结构解析、不渲染图片的测试上下文。
// 返回值：无；正常扫描件误拒或资源越界图片被接受时失败。
func TestExternalPDFImageBudgets(t *testing.T) {
	for _, sample := range []struct {
		name             string
		width, height    int
		indirect, reject bool
	}{
		{"scan", 32, 32, false, false},
		{"render-limit", 4096, 4096, false, false},
		{"render-over", 4097, 4096, false, true},
		{"pixel-over", 8193, 4096, false, true},
		{"indirect-over", 8193, 4096, true, true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			body := admissionImagePDF(t, sample.width, sample.height, sample.indirect)
			err := validatePolicyFixture(body)
			if sample.reject && err == nil {
				t.Fatal("超预算静态图片被接受")
			}
			if !sample.reject && err != nil {
				t.Fatalf("正常扫描件被拒绝: %v", err)
			}
		})
	}
}

// TestExternalPDFSampleMemoryBudget 验证高精度色彩样本的原始行预算及遮罩兼容。
// 参数：t为离线准入测试上下文。
// 返回值：无；精确上限误拒、越界误收或合法遮罩不兼容时失败。
func TestExternalPDFSampleMemoryBudget(t *testing.T) {
	for _, sample := range []struct {
		name                                 string
		width, height, precision, components int
		colorSpace                           string
		reject                               bool
	}{
		{"raw-limit", 2048, 4096, 16, 4, "/ColorSpace /DeviceCMYK", false},
		{"raw-over", 2049, 4096, 16, 4, "/ColorSpace /DeviceCMYK", true},
		{"mask", 32, 32, 1, 1, "/ImageMask true", false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			body := admissionImagePDFSamples(t, sample.width, sample.height, false,
				sample.precision, sample.components, sample.colorSpace)
			err := validatePolicyFixture(body)
			if sample.reject && err == nil {
				t.Fatal("解压样本超预算图片被接受")
			}
			if !sample.reject && err != nil {
				t.Fatalf("合法样本或精确上限被拒绝: %v", err)
			}
		})
	}
}

// TestExternalPDFUnreferencedImageBudget 验证未挂页面资源的图片也不能绕过预算。
// 参数：t为只进行结构检查的测试上下文。
// 返回值：无；独立图片对象超预算仍被接受时失败。
func TestExternalPDFUnreferencedImageBudget(t *testing.T) {
	body := admissionImagePDF(t, 8193, 4096, false)
	resource := []byte("/XObject << /Scan 4 0 R >>")
	// 同长空白删除资源引用，保留真实像素及全部xref偏移。
	body = bytes.Replace(body, resource, bytes.Repeat([]byte(" "), len(resource)), 1)
	if err := validatePolicyFixture(body); err == nil {
		t.Fatal("未挂页面的超预算图片被接受")
	}
}

// admissionImagePDF 生成有真实压缩像素且无文字层的静态扫描夹具。
// 参数：t为测试上下文，width和height为位图尺寸，indirect决定尺寸是否间接引用。
// 返回值：可结构解析的单页PDF；仅本地校验，不渲染或上传。
func admissionImagePDF(t *testing.T, width, height int, indirect bool) []byte {
	return admissionImagePDFSamples(t, width, height, indirect, 1, 1,
		"/ColorSpace /DeviceGray")
}

// admissionImagePDFSamples 为颜色精度及遮罩用例生成真实压缩扫描数据。
// 参数：t为测试上下文，其余参数决定尺寸引用、采样精度、通道数及颜色或遮罩声明。
// 返回值：合法像素流与交叉引用齐备的单页PDF；不调用图像编解码或渲染器。
func admissionImagePDFSamples(t *testing.T, width, height int, indirect bool,
	precision, components int, colorSpace string) []byte {
	t.Helper()
	var pixels bytes.Buffer
	compressed := zlib.NewWriter(&pixels)
	row := make([]byte, (width*components*precision+7)/8)
	for index := 0; index < height; index++ {
		if _, err := compressed.Write(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	dimensions := fmt.Sprintf("/Width %d /Height %d", width, height)
	if indirect {
		dimensions = "/Width 6 0 R /Height 7 0 R"
	}
	content := "q 200 0 0 200 0 0 cm /Scan Do Q"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] " +
			"/Resources << /XObject << /Scan 4 0 R >> >> /Contents 5 0 R >>",
		fmt.Sprintf("<< /Type /XObject /Subtype /Image %s %s "+
			"/BitsPerComponent %d /Filter /FlateDecode /Length %d >>\nstream\n%s\nendstream",
			dimensions, colorSpace, precision, pixels.Len(), pixels.String()),
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
	}
	if indirect {
		objects = append(objects, fmt.Sprint(width), fmt.Sprint(height))
	}
	return admissionPDF(objects, "")
}

// admissionPDF 为本包夹具建立准确交叉引用及合法注释位置。
// 参数：objects为按对象号排列的静态对象，comment为对象前的PDF注释。
// 返回值：保留原始对象和流字节的完整PDF。
func admissionPDF(objects []string, comment string) []byte {
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

// validatePolicyFixture 使用原件实际大小调用独立网关策略。
// 参数：body为只在本地解析的合成原件。
// 返回值：静态对象准入结果；不改写字节或执行内容。
func validatePolicyFixture(body []byte) error { return Validate(body, int64(len(body))) }
