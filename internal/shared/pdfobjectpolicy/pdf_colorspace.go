package pdfobjectpolicy

import (
	"fmt"

	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// pdfImageColorComponents 有界解析图片采样通道，禁止循环引用及Indexed嵌套基色。
// 参数：table为引用表，object为色彩声明，path记录祖先引用，depth及indexed约束遍历。
// 返回值：采样通道数；省略声明为0，非法声明、循环或超深引用返回错误。
func pdfImageColorComponents(table *model.XRefTable, object types.Object,
	path map[types.IndirectRef]bool, depth int, indexed bool) (int, error) {
	if depth > 64 {
		return 0, fmt.Errorf("PDF图片色彩空间超出深度上限")
	}
	if reference, ok := object.(types.IndirectRef); ok {
		if path[reference] {
			return 0, fmt.Errorf("PDF图片色彩空间存在循环引用")
		}
		path[reference] = true
		defer delete(path, reference)
		resolved, err := table.Dereference(reference)
		if err != nil || resolved == nil {
			return 0, fmt.Errorf("PDF图片色彩空间引用无效")
		}
		return pdfImageColorComponents(table, resolved, path, depth+1, indexed)
	}
	if object == nil {
		return 0, nil
	}
	if name, ok := object.(types.Name); ok {
		return pdfNamedColorComponents(name)
	}
	array, ok := object.(types.Array)
	if !ok || len(array) < 2 {
		return 0, fmt.Errorf("PDF图片色彩空间声明无效")
	}
	name, err := table.Dereference(array[0])
	if err != nil {
		return 0, fmt.Errorf("PDF图片色彩空间类型无效")
	}
	return pdfArrayColorComponents(table, array, name, path, depth, indexed)
}

// pdfNamedColorComponents 返回设备或CIE色彩名称对应的采样通道数。
// 参数：name为已解引用的色彩名称。
// 返回值：已知名称的通道数；不支持的名称返回错误。
func pdfNamedColorComponents(name types.Name) (int, error) {
	switch name {
	case "DeviceGray", "CalGray", "Separation":
		return 1, nil
	case "DeviceRGB", "CalRGB", "Lab":
		return 3, nil
	case "DeviceCMYK":
		return 4, nil
	}
	return 0, fmt.Errorf("PDF图片色彩空间名称无效")
}

// pdfArrayColorComponents 读取颜色数组的采样信息并限制可递归的Indexed基色。
// 参数：table和array为解析数据，name为类型，path及depth跟踪祖先，indexed允许索引色。
// 返回值：采样通道数；循环、嵌套索引色或必要字段非法时返回错误。
func pdfArrayColorComponents(table *model.XRefTable, array types.Array, name types.Object,
	path map[types.IndirectRef]bool, depth int, indexed bool) (int, error) {
	switch name {
	case types.Name("Indexed"):
		if !indexed || len(array) != 4 {
			return 0, fmt.Errorf("PDF图片Indexed基色不能使用Indexed")
		}
		if _, err := pdfImageColorComponents(table, array[1], path, depth+1, false); err != nil {
			return 0, err
		}
		return 1, nil // 索引像素存储一个采样值，而非基色的通道数。
	case types.Name("ICCBased"):
		return pdfICCColorComponents(table, array)
	case types.Name("DeviceN"):
		names, err := table.DereferenceArray(array[1])
		if err != nil || len(names) == 0 {
			return 0, fmt.Errorf("PDF图片DeviceN通道声明无效")
		}
		return len(names), nil
	}
	if colorName, ok := name.(types.Name); ok {
		return pdfNamedColorComponents(colorName)
	}
	return 0, fmt.Errorf("PDF图片色彩空间数组无效")
}

// pdfICCColorComponents 读取ICC配置的受限通道数而不解码配置流。
// 参数：table为引用表，array为ICCBased数组。
// 返回值：合法的1、3或4通道；缺失配置、非法引用或通道数返回错误。
func pdfICCColorComponents(table *model.XRefTable, array types.Array) (int, error) {
	if len(array) != 2 {
		return 0, fmt.Errorf("PDF图片ICC配置声明无效")
	}
	profile, _, err := table.DereferenceStreamDict(array[1])
	if err != nil || profile == nil {
		return 0, fmt.Errorf("PDF图片ICC配置引用无效")
	}
	channels, err := table.DereferenceInteger(profile.Dict["N"])
	if err != nil || channels == nil || *channels != 1 && *channels != 3 && *channels != 4 {
		return 0, fmt.Errorf("PDF图片ICC通道声明无效")
	}
	return int(*channels), nil
}
