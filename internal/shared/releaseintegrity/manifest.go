package releaseintegrity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
)

const (
	// ManifestSchema 是签名发布清单所用的项目版本化
	// schema，刻意不采用 Sigstore/in-toto 媒体类型。
	ManifestSchema = "urn:uip:release-manifest:v1"
	// TargetPlatform 是第一版发布完整性策略唯一
	// 接受的平台。
	TargetPlatform = "linux/amd64"
)

var (
	sha256HexRe  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	digestRe     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	releaseIDRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	appCodeRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	serviceIDRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	imageNameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	envCodeRe    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	allowedRoles = map[string]bool{
		"api":              true,
		"worker":           true,
		"migration":        true,
		"initializer":      true,
		"infrastructure":   true,
		"directory-sync":   true,
		"condition-worker": true,
	}
	artifactTypes = map[string]bool{
		"image-package":     true,
		"asset":             true,
		"script":            true,
		"host-dependencies": true,
		"template":          true,
		"checksum":          true,
		"provenance":        true,
		"other":             true,
	}
)

// Manifest 是单个生产发布的签名描述，只包含
// 软件事实，不含客户标识、实例标识、商业有效期、
// 凭据、环境变量值或私钥。
type Manifest struct {
	Schema              string               `json:"schema"`
	ReleaseID           string               `json:"release_id"`
	Channel             string               `json:"channel"`
	ReleaseSequence     int64                `json:"release_sequence"`
	TargetPlatform      string               `json:"target_platform"`
	BuildInputsSHA256   string               `json:"build_inputs_sha256"`
	Compatibility       Compatibility        `json:"compatibility"`
	Artifacts           []Artifact           `json:"artifacts"`
	Profiles            []Profile            `json:"profiles"`
	Applications        []Application        `json:"applications"`
	Images              []Image              `json:"images"`
	LegacyCompatibility *LegacyCompatibility `json:"legacy_compatibility,omitempty"`
}

type Compatibility struct {
	VerifierPolicy           string         `json:"verifier_policy"`
	MinAPIReleaseIntegrity   string         `json:"min_api_release_integrity"`
	MinAgentReleaseIntegrity string         `json:"min_agent_release_integrity"`
	CatalogVersion           int            `json:"catalog_version"`
	RegistrationProtocols    map[string]int `json:"registration_protocols"`
	SharedControlPlaneSHA256 string         `json:"shared_control_plane_sha256"`
	MigrationPreconditions   []string       `json:"migration_preconditions,omitempty"`
}

type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Type   string `json:"type"`
}

type Profile struct {
	Path             string `json:"path"`
	SHA256           string `json:"sha256"`
	NormalizedSHA256 string `json:"normalized_sha256"`
	PolicyVersion    int    `json:"policy_version"`
}

type Application struct {
	Code        string      `json:"code"`
	Environment string      `json:"environment"`
	Components  []Component `json:"components"`
}

type Component struct {
	ServiceID            string `json:"service_id"`
	Role                 string `json:"role"`
	Conditional          bool   `json:"conditional"`
	Image                string `json:"image"`
	RegistrationProtocol int    `json:"registration_protocol"`
	CatalogVersion       int    `json:"catalog_version"`
	Replicas             int    `json:"replicas"`
	CommandPolicy        string `json:"command_policy"`
}

type Image struct {
	Name             string `json:"name"`
	Platform         string `json:"platform"`
	Source           string `json:"source"`
	Package          string `json:"package,omitempty"`
	ArchiveMember    string `json:"archive_member,omitempty"`
	ConfigDigest     string `json:"config_digest,omitempty"`
	ManifestDigest   string `json:"manifest_digest,omitempty"`
	IndexDigest      string `json:"index_digest,omitempty"`
	Provenance       string `json:"provenance,omitempty"`
	ProvenanceBundle string `json:"provenance_bundle,omitempty"`
}

func (i Image) typedDigests() []string {
	out := make([]string, 0, 3)
	for _, digest := range []string{i.ConfigDigest, i.ManifestDigest, i.IndexDigest} {
		if digest != "" {
			out = append(out, digest)
		}
	}
	return out
}

type LegacyCompatibility struct {
	V1ApprovalSHA256 string `json:"v1_approval_sha256,omitempty"`
	Purpose          string `json:"purpose,omitempty"`
}

// ValidationOptions 控制依赖调用阶段的策略检查。
type ValidationOptions struct {
	RequireProvenance bool
	RequireProd       bool
}

func DefaultValidationOptions() ValidationOptions {
	return ValidationOptions{RequireProvenance: true, RequireProd: true}
}

// ParseManifest 严格解码并校验发布清单。调用前必须
// 已对同一份原始字节完成签名验证；
// 仅解析不能建立信任。
func ParseManifest(raw []byte, options ValidationOptions) (*Manifest, error) {
	var manifest Manifest
	if err := decodeStrictJSON(raw, &manifest); err != nil {
		return nil, failDetails(CodeReleaseInvalid, "发布清单不是严格 JSON", map[string]any{"error": err.Error()})
	}
	if err := manifest.Validate(options); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func (m *Manifest) Validate(options ValidationOptions) error {
	if m == nil {
		return fail(CodeReleaseInvalid, "发布清单为空")
	}
	if m.Schema != ManifestSchema {
		return fail(CodeReleaseInvalid, "发布清单 schema 不支持：%s", m.Schema)
	}
	if !releaseIDRe.MatchString(m.ReleaseID) {
		return fail(CodeReleaseInvalid, "release_id 格式不正确")
	}
	if m.Channel != "production" && m.Channel != "development" {
		return fail(CodeReleaseInvalid, "channel 必须是 production 或 development")
	}
	if m.Channel == "production" && options.RequireProd && m.ReleaseSequence <= 0 {
		return fail(CodeReleaseInvalid, "production 发布序列必须是正整数")
	}
	if m.ReleaseSequence <= 0 {
		return fail(CodeReleaseInvalid, "release_sequence 必须是正整数")
	}
	if m.TargetPlatform != TargetPlatform {
		return failDetails(CodePlatformMismatch, "发布目标平台不匹配", map[string]any{
			"expected": TargetPlatform, "actual": m.TargetPlatform,
		})
	}
	if !sha256HexRe.MatchString(m.BuildInputsSHA256) {
		return fail(CodeReleaseInvalid, "build_inputs_sha256 必须是 64 位小写十六进制")
	}
	if err := validateCompatibility(m.Compatibility); err != nil {
		return err
	}
	if err := validateArtifacts(m.Artifacts); err != nil {
		return err
	}
	if err := validateProfiles(m.Profiles); err != nil {
		return err
	}
	if err := validateImages(m.Images, options); err != nil {
		return err
	}
	if err := validateApplications(m.Applications, m.Images, options); err != nil {
		return err
	}
	return nil
}

func validateCompatibility(c Compatibility) error {
	if c.VerifierPolicy == "" {
		return fail(CodeReleaseInvalid, "compatibility.verifier_policy 缺失")
	}
	if c.MinAPIReleaseIntegrity == "" || c.MinAgentReleaseIntegrity == "" {
		return fail(CodeReleaseInvalid, "API/Agent 最低发布完整性能力缺失")
	}
	if c.CatalogVersion <= 0 {
		return fail(CodeReleaseInvalid, "catalog_version 必须是正整数")
	}
	if len(c.RegistrationProtocols) == 0 {
		return fail(CodeReleaseInvalid, "registration_protocols 不能为空")
	}
	for app, protocol := range c.RegistrationProtocols {
		if !appCodeRe.MatchString(app) {
			return fail(CodeReleaseInvalid, "registration_protocols 中存在非法应用编码：%s", app)
		}
		if protocol != 1 && protocol != 2 {
			return fail(CodeReleaseInvalid, "应用 %s 的登记协议不支持：%d", app, protocol)
		}
	}
	if !sha256HexRe.MatchString(c.SharedControlPlaneSHA256) {
		return fail(CodeReleaseInvalid, "shared_control_plane_sha256 必须是 64 位小写十六进制")
	}
	return nil
}

func validateArtifacts(artifacts []Artifact) error {
	if len(artifacts) == 0 {
		return fail(CodeReleaseInvalid, "artifacts 不能为空")
	}
	seen := make(map[string]bool, len(artifacts))
	for _, artifact := range artifacts {
		if err := safeRelativePath(artifact.Path); err != nil {
			return failDetails(CodeReleaseInvalid, "artifact 路径不安全", map[string]any{
				"path": artifact.Path, "error": err.Error(),
			})
		}
		if seen[artifact.Path] {
			return fail(CodeReleaseInvalid, "artifact 路径重复：%s", artifact.Path)
		}
		seen[artifact.Path] = true
		if !sha256HexRe.MatchString(artifact.SHA256) {
			return fail(CodeReleaseInvalid, "artifact 摘要格式不正确：%s", artifact.Path)
		}
		if artifact.Size <= 0 {
			return fail(CodeReleaseInvalid, "artifact 长度必须为正：%s", artifact.Path)
		}
		if !artifactTypes[artifact.Type] {
			return fail(CodeReleaseInvalid, "artifact 类型不支持：%s", artifact.Type)
		}
	}
	return nil
}

func validateProfiles(profiles []Profile) error {
	if len(profiles) != 6 {
		return fail(CodeComponentSetMismatch, "发布清单必须覆盖六份原始 profile，实际 %d", len(profiles))
	}
	seen := make(map[string]bool, len(profiles))
	for _, profile := range profiles {
		if err := safeRelativePath(profile.Path); err != nil {
			return failDetails(CodeReleaseInvalid, "profile 路径不安全", map[string]any{
				"path": profile.Path, "error": err.Error(),
			})
		}
		if seen[profile.Path] {
			return fail(CodeReleaseInvalid, "profile 路径重复：%s", profile.Path)
		}
		seen[profile.Path] = true
		if !sha256HexRe.MatchString(profile.SHA256) {
			return fail(CodeReleaseInvalid, "profile 摘要格式不正确：%s", profile.Path)
		}
		if !sha256HexRe.MatchString(profile.NormalizedSHA256) {
			return fail(CodeReleaseInvalid, "profile 规范化摘要格式不正确：%s", profile.Path)
		}
		if profile.PolicyVersion <= 0 {
			return fail(CodeReleaseInvalid, "profile 策略版本必须是正整数：%s", profile.Path)
		}
	}
	return nil
}

func validateImages(images []Image, options ValidationOptions) error {
	if len(images) == 0 {
		return fail(CodeReleaseInvalid, "images 不能为空")
	}
	seen := make(map[string]bool, len(images))
	for _, image := range images {
		if !imageNameRe.MatchString(image.Name) {
			return fail(CodeReleaseInvalid, "镜像名称不合法：%s", image.Name)
		}
		if seen[image.Name] {
			return fail(CodeReleaseInvalid, "镜像名称重复：%s", image.Name)
		}
		seen[image.Name] = true
		if image.Platform != TargetPlatform {
			return failDetails(CodePlatformMismatch, "镜像平台不匹配", map[string]any{
				"image": image.Name, "expected": TargetPlatform, "actual": image.Platform,
			})
		}
		digests := image.typedDigests()
		if len(digests) == 0 {
			return fail(CodeReleaseInvalid, "镜像缺少有类型的摘要：%s", image.Name)
		}
		for _, digest := range digests {
			if !digestRe.MatchString(digest) {
				return fail(CodeReleaseInvalid, "镜像摘要格式不正确：%s", image.Name)
			}
		}
		if image.Source == "" {
			return fail(CodeReleaseInvalid, "镜像来源不能为空：%s", image.Name)
		}
		if image.Package != "" {
			if err := safeRelativePath(image.Package); err != nil {
				return fail(CodeReleaseInvalid, "镜像包路径不安全：%s", image.Package)
			}
		}
		if image.ArchiveMember != "" {
			if !safeArchiveMember(image.ArchiveMember) {
				return fail(CodeReleaseInvalid, "镜像归档成员路径不安全：%s", image.ArchiveMember)
			}
		}
		if options.RequireProvenance {
			if image.Provenance == "" || image.ProvenanceBundle == "" {
				return fail(CodeProvenanceInvalid, "镜像缺少构建证明：%s", image.Name)
			}
			if err := safeRelativePath(image.Provenance); err != nil {
				return fail(CodeProvenanceInvalid, "构建证明路径不安全：%s", image.Provenance)
			}
			if err := safeRelativePath(image.ProvenanceBundle); err != nil {
				return fail(CodeProvenanceInvalid, "构建证明 bundle 路径不安全：%s", image.ProvenanceBundle)
			}
		}
	}
	return nil
}

func validateApplications(applications []Application, images []Image, options ValidationOptions) error {
	if len(applications) == 0 {
		return fail(CodeComponentSetMismatch, "applications 不能为空")
	}
	imageNames := make(map[string]bool, len(images))
	for _, image := range images {
		imageNames[image.Name] = true
	}
	seenApps := make(map[string]bool, len(applications))
	for _, application := range applications {
		if !appCodeRe.MatchString(application.Code) {
			return fail(CodeReleaseInvalid, "应用编码不合法：%s", application.Code)
		}
		if options.RequireProd && application.Environment != "prod" {
			return fail(CodeComponentSetMismatch, "生产发布只能包含 prod 环境：%s/%s", application.Code, application.Environment)
		}
		if !envCodeRe.MatchString(application.Environment) {
			return fail(CodeReleaseInvalid, "环境编码不合法：%s", application.Environment)
		}
		key := application.Code + "/" + application.Environment
		if seenApps[key] {
			return fail(CodeReleaseInvalid, "应用/环境重复：%s", key)
		}
		seenApps[key] = true
		if len(application.Components) == 0 {
			return fail(CodeComponentSetMismatch, "应用没有运行组件：%s", key)
		}
		seenServices := make(map[string]bool, len(application.Components))
		for _, component := range application.Components {
			if !serviceIDRe.MatchString(component.ServiceID) {
				return fail(CodeReleaseInvalid, "服务标识不合法：%s", component.ServiceID)
			}
			if seenServices[component.ServiceID] {
				return fail(CodeReleaseInvalid, "应用 %s 内服务重复：%s", key, component.ServiceID)
			}
			seenServices[component.ServiceID] = true
			if !allowedRoles[component.Role] {
				return fail(CodeReleaseInvalid, "服务角色不支持：%s", component.Role)
			}
			if !imageNames[component.Image] {
				return failDetails(CodeComponentSetMismatch, "服务引用了不在 images 中的镜像", map[string]any{
					"application": key, "service": component.ServiceID, "image": component.Image,
				})
			}
			if component.RegistrationProtocol != 1 && component.RegistrationProtocol != 2 {
				return fail(CodeReleaseInvalid, "服务登记协议不支持：%s", component.ServiceID)
			}
			if component.CatalogVersion <= 0 {
				return fail(CodeReleaseInvalid, "服务目录版本必须是正整数：%s", component.ServiceID)
			}
			if component.Replicas <= 0 {
				return fail(CodeReleaseInvalid, "服务副本数必须是正整数：%s", component.ServiceID)
			}
		}
	}
	return nil
}

func safeRelativePath(value string) error {
	if value == "" {
		return fmt.Errorf("路径为空")
	}
	if strings.ContainsAny(value, "\x00\\") {
		return fmt.Errorf("路径包含禁止字符")
	}
	if strings.HasPrefix(value, "/") {
		return fmt.Errorf("路径不能是绝对路径")
	}
	cleaned := path.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("路径不能逃逸介质目录")
	}
	if cleaned != value {
		return fmt.Errorf("路径必须已规范化")
	}
	return nil
}

func safeArchiveMember(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\x00\\") {
		return false
	}
	cleaned := path.Clean(value)
	return cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, "../")
}

func decodeStrictJSON(raw []byte, target any) error {
	if err := rejectDuplicateKeys(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("存在尾随 JSON 数据")
		}
		return err
	}
	return nil
}

// rejectDuplicateKeys 在 encoding/json 静默覆盖重复对象键之前
// 将其检出，通过遍历令牌并跟踪对象和数组嵌套完成检查。
func rejectDuplicateKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	type frame struct {
		object    bool
		keys      map[string]bool
		expectKey bool
	}
	var stack []*frame
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch value := token.(type) {
		case json.Delim:
			switch value {
			case '{':
				if len(stack) > 0 && stack[len(stack)-1].object {
					stack[len(stack)-1].expectKey = false
				}
				stack = append(stack, &frame{object: true, keys: map[string]bool{}, expectKey: true})
			case '[':
				if len(stack) > 0 && stack[len(stack)-1].object {
					stack[len(stack)-1].expectKey = false
				}
				stack = append(stack, &frame{})
			case '}', ']':
				if len(stack) == 0 {
					return fmt.Errorf("JSON 结构不匹配")
				}
				stack = stack[:len(stack)-1]
				if len(stack) > 0 && stack[len(stack)-1].object {
					stack[len(stack)-1].expectKey = true
				}
			}
		case string:
			if len(stack) > 0 && stack[len(stack)-1].object && stack[len(stack)-1].expectKey {
				if stack[len(stack)-1].keys[value] {
					return fmt.Errorf("重复的 JSON 键：%s", value)
				}
				stack[len(stack)-1].keys[value] = true
				stack[len(stack)-1].expectKey = false
			} else if len(stack) > 0 && stack[len(stack)-1].object {
				stack[len(stack)-1].expectKey = true
			}
		default:
			if len(stack) > 0 && stack[len(stack)-1].object {
				stack[len(stack)-1].expectKey = true
			}
		}
	}
}
