package pdfobjectpolicy

import (
	"bytes"
	"context"
	"fmt"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
	"time"
)

var pdfValidationSlots = make(chan struct{}, 2)

// Validate 有界解析可疑PDF对象，区分静态字节和禁止的活动内容。
// 参数：body为原件，maxInputBytes为调用方已验证的用途或实际大小上限。
// 返回值：有效静态文档为nil；损坏、活动对象、加密或资源越界返回中文错误。
func Validate(body []byte, maxInputBytes int64) error {
	if maxInputBytes <= 0 || int64(len(body)) > maxInputBytes {
		return fmt.Errorf("PDF输入超出已验证大小上限")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case pdfValidationSlots <- struct{}{}:
		defer func() { <-pdfValidationSlots }()
	case <-ctx.Done():
		return fmt.Errorf("PDF校验繁忙，请稍后重试")
	}
	conf := model.NewStatelessConfiguration()
	conf.Cmd, conf.ValidationMode, conf.Offline = model.VALIDATE, model.ValidationStrict, true
	conf.ValidateLinks, conf.Optimize, conf.DecodeAllStreams = false, false, false
	conf.Limits = model.ResourceLimits{MaxInputBytes: maxInputBytes, MaxObjectBytes: 4 << 20,
		MaxStreamBytes: maxInputBytes, MaxDecodeBytes: 64 << 20, MaxImageBytes: 64 << 20, MaxImagePixels: 32 << 20,
		MaxObjectCount: 100000, MaxObjectStreamCount: 20000, MaxObjectStreamFirst: 1 << 20,
		MaxXRefEntries: 100000, MaxRecursionDepth: 64}
	parsed, err := api.ReadContext(ctx, bytes.NewReader(body), conf)
	if err != nil {
		return fmt.Errorf("PDF结构损坏、加密或超出解析资源上限")
	}
	if parsed.Encrypt != nil {
		return fmt.Errorf("PDF含不支持的加密内容")
	}
	budget := 1000000
	// 先检查解码后对象，避免校验器忽略未知键或修复字典后遗漏活动内容。
	for _, entry := range parsed.Table {
		if entry == nil || entry.Free {
			continue
		}
		if err = inspectPDFObject(entry.Object, 0, &budget); err != nil {
			return err
		}
		if stream, ok := entry.Object.(types.StreamDict); ok {
			if err = validatePDFImageBudget(parsed.XRefTable, stream); err != nil {
				return err
			}
		}
	}
	if err = api.ValidateContext(ctx, parsed); err != nil || parsed.PageCount < 1 {
		return fmt.Errorf("PDF页面结构不完整或不符合接入要求")
	}
	return nil
}

// forbiddenPDFName 对解码后的字典名及动作名执行统一准入策略。
// 参数：name为PDF名称对象的原始或已解码值。
// 返回值：禁止特征或非法名称为true。
func forbiddenPDFName(name string) bool {
	decoded, err := types.DecodeName(name)
	if err != nil {
		return true
	}
	switch decoded {
	case "JavaScript", "JS", "EmbeddedFile", "EmbeddedFiles", "Launch", "Encrypt":
		return true
	}
	return false
}

// inspectPDFObject 有界遍历解码对象，覆盖名称转义及压缩对象流。
// 参数：object为解析对象，depth为嵌套深度，budget限制总节点数。
// 返回值：禁止对象或资源越界错误；不解引用循环引用、不执行任何动作。
func inspectPDFObject(object types.Object, depth int, budget *int) error {
	*budget--
	if *budget < 0 || depth > 64 {
		return fmt.Errorf("PDF对象数量或嵌套超出接入上限")
	}
	switch value := object.(type) {
	case types.Name:
		if forbiddenPDFName(string(value)) {
			return fmt.Errorf("PDF含不支持的活动、嵌入或加密内容")
		}
	case types.Dict:
		for key, child := range value {
			if forbiddenPDFName(key) {
				return fmt.Errorf("PDF含不支持的活动、嵌入或加密内容")
			}
			if err := inspectPDFObject(child, depth+1, budget); err != nil {
				return err
			}
		}
	case types.Array:
		for _, child := range value {
			if err := inspectPDFObject(child, depth+1, budget); err != nil {
				return err
			}
		}
	case types.StreamDict:
		return inspectPDFObject(value.Dict, depth+1, budget)
	case types.ObjectStreamDict:
		if err := inspectPDFObject(value.Dict, depth+1, budget); err != nil {
			return err
		}
		return inspectPDFObject(value.ObjArray, depth+1, budget)
	}
	return nil
}
