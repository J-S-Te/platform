package pdfobjectpolicy

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"runtime/debug"
	"syscall"
	"testing"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// TestExternalPDFColorSpaceCycle 通过独立进程和栈护栏复现色彩空间递归风险。
// 参数：t为主测试进程上下文，危险旧实现只在受限子进程执行。
// 返回值：无；子进程崩溃、超时或接受循环色彩空间时失败。
func TestExternalPDFColorSpaceCycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPDFColorSpaceCycleChild$")
	cmd.Env = append(os.Environ(), "PDF_COLORSPACE_CHILD=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		if len(output) > 2048 {
			output = output[:2048]
		}
		t.Fatalf("受限色彩空间检查失败: %v\n%s", err, output)
	}
}

// TestPDFColorSpaceCycleChild 在可控CPU、墙钟时间和栈空间内验证循环拒绝。
// 参数：t为只由父测试显式启动的子进程上下文。
// 返回值：无；接受单环或双环时失败，旧递归被栈护栏终止时父测试失败。
func TestPDFColorSpaceCycleChild(t *testing.T) {
	if os.Getenv("PDF_COLORSPACE_CHILD") != "1" {
		t.Skip("仅在受限独立进程运行")
	}
	debug.SetMaxStack(128 << 10)
	if err := syscall.Setrlimit(syscall.RLIMIT_CPU, &syscall.Rlimit{Cur: 2, Max: 2}); err != nil {
		t.Fatal(err)
	}
	cases := [][]string{
		{"[/Indexed 6 0 R 1 <000000ffffff>]"},
		{"[/Indexed 7 0 R 1 <000000ffffff>]", "[/Indexed 6 0 R 1 <000000ffffff>]"},
	}
	deep := []string{}
	for index := 0; index < 70; index++ {
		deep = append(deep, fmt.Sprintf("%d 0 R", index+7))
	}
	cases = append(cases, append(deep, "/DeviceGray"))
	for _, objects := range cases {
		if err := validatePolicyFixture(admissionColorSpacePDF(objects)); err == nil {
			t.Fatal("循环色彩空间被接受")
		}
	}
}

// TestExternalPDFSupportedColorSpaces 验证合法Indexed、ICCBased与DeviceN仍可准入。
// 参数：t为真实PDF解析测试上下文。
// 返回值：无；正常颜色声明被误拒时失败。
func TestExternalPDFSupportedColorSpaces(t *testing.T) {
	profile := make([]byte, 132)
	binary.BigEndian.PutUint32(profile[:4], uint32(len(profile)))
	copy(profile[12:16], "mntr")
	copy(profile[16:20], "GRAY")
	copy(profile[20:24], "XYZ ")
	copy(profile[36:40], "acsp")
	for _, sample := range []struct {
		name    string
		objects []string
	}{
		{"Indexed", []string{"[/Indexed /DeviceRGB 1 <000000ffffff>]"}},
		{"ICCBased", []string{
			"[/ICCBased 7 0 R]",
			fmt.Sprintf("<< /N 1 /Alternate /DeviceGray /Length %d >>\nstream\n%s\nendstream",
				len(profile), profile),
		}},
		{"DeviceN", []string{
			"[/DeviceN [/Spot] /DeviceRGB 7 0 R]",
			"<< /FunctionType 2 /Domain [0 1] /C0 [0 0 0] /C1 [1 1 1] /N 1 >>",
		}},
	} {
		t.Run(sample.name, func(t *testing.T) {
			if err := validatePolicyFixture(admissionColorSpacePDF(sample.objects)); err != nil {
				t.Fatalf("合法颜色空间被拒绝: %v", err)
			}
		})
	}
}

// TestPDFColorSpaceReferenceDepth 验证本包解析器的间接引用深度边界。
// 参数：t为有界解析器测试上下文，引用表仅包含确定有限的名称链。
// 返回值：无；64层合法链误拒或65层链未拒绝时失败。
func TestPDFColorSpaceReferenceDepth(t *testing.T) {
	for _, depth := range []int{64, 65} {
		table := &model.XRefTable{Table: make(map[int]*model.XRefTableEntry)}
		for index := 1; index <= depth; index++ {
			var object types.Object = types.Name("DeviceGray")
			if index < depth {
				object = *types.NewIndirectRef(index+1, 0)
			}
			table.Table[index] = &model.XRefTableEntry{Object: object}
		}
		channels, err := pdfImageColorComponents(table, *types.NewIndirectRef(1, 0),
			make(map[types.IndirectRef]bool), 0, true)
		if depth == 64 && (err != nil || channels != 1) {
			t.Fatalf("64层合法引用失败: channels=%d err=%v", channels, err)
		}
		if depth == 65 && err == nil {
			t.Fatal("65层色彩空间引用未被拒绝")
		}
	}
}

// admissionColorSpacePDF 生成以外部色彩空间对象驱动的一像素静态图像PDF。
// 参数：colorObjects为从对象6开始的色彩空间及其依赖对象。
// 返回值：含真实一字节像素、准确xref且无动作的PDF，不渲染或上传。
func admissionColorSpacePDF(colorObjects []string) []byte {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 10 10] " +
			"/Resources << /XObject << /Image 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /XObject /Subtype /Image /Width 1 /Height 1 /ColorSpace 6 0 R " +
			"/BitsPerComponent 8 /Length 1 >>\nstream\n\x00\nendstream",
		"<< /Length 9 >>\nstream\n/Image Do\nendstream",
	}
	return admissionPDF(append(objects, colorObjects...), "")
}
