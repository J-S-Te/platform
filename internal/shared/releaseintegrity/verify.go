package releaseintegrity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	maxManifestBytes  = 4 << 20
	maxBundleBytes    = 8 << 20
	maxStatementBytes = 8 << 20
	maxArtifacts      = 4096
	maxTotalBytes     = int64(64) << 30
)

// VerifyOptions 描述只读验证。MediaDir 是只读
// 交付目录；TrustDir 由宿主机控制，必须独立于
// 交付介质。Applications 为可选项，设置后计划仅包含指定
// 应用，但仍检查完整的六类配置和镜像边界。
type VerifyOptions struct {
	MediaDir          string
	TrustDir          string
	Applications      []string
	RequireProvenance bool
}

func DefaultVerifyOptions(mediaDir, trustDir string) VerifyOptions {
	return VerifyOptions{
		MediaDir:          mediaDir,
		TrustDir:          trustDir,
		RequireProvenance: true,
	}
}

type VerifiedArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Type   string `json:"type"`
}

type VerifiedProfile struct {
	Path             string `json:"path"`
	SHA256           string `json:"sha256"`
	NormalizedSHA256 string `json:"normalized_sha256"`
	PolicyVersion    int    `json:"policy_version"`
}

type VerifiedImage struct {
	Name             string   `json:"name"`
	Platform         string   `json:"platform"`
	ConfigDigest     string   `json:"config_digest,omitempty"`
	ManifestDigest   string   `json:"manifest_digest,omitempty"`
	IndexDigest      string   `json:"index_digest,omitempty"`
	Provenance       string   `json:"provenance,omitempty"`
	ProvenanceBundle string   `json:"provenance_bundle,omitempty"`
	ProvenanceKeyID  string   `json:"provenance_key_id,omitempty"`
	Subjects         []string `json:"subjects,omitempty"`
}

type VerifiedComponent struct {
	ServiceID            string `json:"service_id"`
	Role                 string `json:"role"`
	Conditional          bool   `json:"conditional"`
	Image                string `json:"image"`
	RegistrationProtocol int    `json:"registration_protocol"`
	CatalogVersion       int    `json:"catalog_version"`
	Replicas             int    `json:"replicas"`
}

type VerifiedApplication struct {
	Code        string              `json:"code"`
	Environment string              `json:"environment"`
	Components  []VerifiedComponent `json:"components"`
}

// VerifiedReleasePlan 是调用方判定已验证发布所包含
// 组件时唯一可使用的值。它是只读投影，
// 不代表许可证、ACK 或部署已经发生的证明。
type VerifiedReleasePlan struct {
	ReleaseID        string                `json:"release_id"`
	ReleaseDigest    string                `json:"release_digest"`
	Channel          string                `json:"channel"`
	Sequence         int64                 `json:"release_sequence"`
	Platform         string                `json:"target_platform"`
	VerifierPolicy   string                `json:"verifier_policy"`
	TrustEpoch       int64                 `json:"trust_epoch"`
	ReleaseKeyID     string                `json:"release_key_id"`
	Artifacts        []VerifiedArtifact    `json:"artifacts"`
	Profiles         []VerifiedProfile     `json:"profiles"`
	Images           []VerifiedImage       `json:"images"`
	Applications     []VerifiedApplication `json:"applications"`
	ProvenanceKeyIDs []string              `json:"provenance_key_ids,omitempty"`
	Status           string                `json:"status"`
}

// VerifyMedia 执行阶段 C 的只读验证：从介质之外加载
// 信任策略，对准确的原始字节验证清单签名，
// 检查全部列出产物及六类配置，并检查每个
// 镜像的 in-toto/SLSA 声明和 DSSE 签名。此过程不
// 导入镜像、写入配置、启动容器或访问网络。
func VerifyMedia(options VerifyOptions) (*VerifiedReleasePlan, error) {
	if options.MediaDir == "" || options.TrustDir == "" {
		return nil, fail(CodeReleaseInvalid, "media/trust 目录不能为空")
	}
	mediaDir, err := safeDirectory(options.MediaDir, "介质目录")
	if err != nil {
		return nil, err
	}
	trustDir, err := safeDirectory(options.TrustDir, "信任目录")
	if err != nil {
		return nil, err
	}
	policy, releaseRing, buildRing, err := LoadTrustPolicy(trustDir)
	if err != nil {
		return nil, err
	}

	manifestRaw, err := readLimitedFile(filepath.Join(mediaDir, "release-manifest.json"), maxManifestBytes)
	if err != nil {
		return nil, fail(CodeTrustUnavailable, "发布清单缺失或不可读")
	}
	bundleRaw, err := readLimitedFile(filepath.Join(mediaDir, "release-manifest.sigstore.json"), maxBundleBytes)
	if err != nil {
		return nil, fail(CodeSignatureInvalid, "发布清单签名缺失或不可读")
	}
	releaseKeyID, err := VerifyBlobBundle(bundleRaw, manifestRaw, releaseRing)
	if err != nil {
		return nil, err
	}
	manifest, err := ParseManifest(manifestRaw, ValidationOptions{
		RequireProvenance: options.RequireProvenance,
		RequireProd:       true,
	})
	if err != nil {
		return nil, err
	}
	if manifest.ReleaseSequence < policy.MinReleaseSequence {
		return nil, failDetails(CodeRollbackRejected, "发布序列低于信任策略最小值", map[string]any{
			"release_sequence": manifest.ReleaseSequence, "minimum": policy.MinReleaseSequence,
		})
	}
	if manifest.Compatibility.VerifierPolicy != policy.Policy {
		return nil, fail(CodeReleaseInvalid, "发布清单与信任策略的 verifier policy 不一致")
	}

	verifiedArtifacts, artifactsByPath, err := verifyArtifacts(mediaDir, manifest.Artifacts)
	if err != nil {
		return nil, err
	}
	verifiedProfiles, err := verifyProfiles(mediaDir, manifest.Profiles)
	if err != nil {
		return nil, err
	}
	verifiedImages, provenanceKeyIDs, err := verifyImages(mediaDir, manifest, artifactsByPath, policy, buildRing, options.RequireProvenance)
	if err != nil {
		return nil, err
	}
	selected := selectApplications(manifest.Applications, options.Applications)
	if len(selected) == 0 {
		return nil, fail(CodeComponentSetMismatch, "没有匹配的应用")
	}
	verifiedApplications := make([]VerifiedApplication, 0, len(selected))
	for _, application := range selected {
		components := make([]VerifiedComponent, 0, len(application.Components))
		for _, component := range application.Components {
			components = append(components, VerifiedComponent{
				ServiceID:            component.ServiceID,
				Role:                 component.Role,
				Conditional:          component.Conditional,
				Image:                component.Image,
				RegistrationProtocol: component.RegistrationProtocol,
				CatalogVersion:       component.CatalogVersion,
				Replicas:             component.Replicas,
			})
		}
		verifiedApplications = append(verifiedApplications, VerifiedApplication{
			Code: application.Code, Environment: application.Environment, Components: components,
		})
	}

	releaseDigest := sha256.Sum256(manifestRaw)
	return &VerifiedReleasePlan{
		ReleaseID:        manifest.ReleaseID,
		ReleaseDigest:    "sha256:" + hex.EncodeToString(releaseDigest[:]),
		Channel:          manifest.Channel,
		Sequence:         manifest.ReleaseSequence,
		Platform:         manifest.TargetPlatform,
		VerifierPolicy:   manifest.Compatibility.VerifierPolicy,
		TrustEpoch:       policy.TrustEpoch,
		ReleaseKeyID:     releaseKeyID,
		Artifacts:        verifiedArtifacts,
		Profiles:         verifiedProfiles,
		Images:           verifiedImages,
		Applications:     verifiedApplications,
		ProvenanceKeyIDs: provenanceKeyIDs,
		Status:           "VERIFIED",
	}, nil
}

// CheckInstalled 验证已安装目标中保留的签名
// 发布清单，不重新计算介质载荷哈希，也不证明运行中
// Compose 投影与清单相符；这些内容由独立且受锁保护的部署
// 检查负责。此检查刻意严格于本地布尔标记：签名、
// 信任策略、发布序列和所选应用 schema 均必须验证通过。
func CheckInstalled(target, trustDir string, applications []string) (*VerifiedReleasePlan, error) {
	return CheckInstalledWithExpected(target, trustDir, applications, "")
}

// CheckInstalledWithExpected 还将验证后的已安装清单
// 绑定到预期发布摘要。预期值为空时仅检查清单；
// 非空时必须是完整 sha256 摘要。
func CheckInstalledWithExpected(target, trustDir string, applications []string, expectedDigest string) (*VerifiedReleasePlan, error) {
	if expectedDigest != "" && !digestRe.MatchString(expectedDigest) {
		return nil, fail(CodeReleaseInvalid, "expected release digest 必须是 sha256:<64hex>")
	}
	targetDir, err := safeDirectory(target, "目标部署目录")
	if err != nil {
		return nil, err
	}
	trustPath, err := safeDirectory(trustDir, "信任目录")
	if err != nil {
		return nil, err
	}
	policy, releaseRing, buildRing, err := LoadTrustPolicy(trustPath)
	if err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(targetDir, "runtime", "release-integrity", "release-manifest.json")
	signaturePath := filepath.Join(targetDir, "runtime", "release-integrity", "release-manifest.sigstore.json")
	projection := true
	if _, err := os.Lstat(manifestPath); err != nil {
		// 兼容将清单存放在目标根目录的目录或介质夹具。
		// 安装器会写入 runtime/release-integrity。
		manifestPath = filepath.Join(targetDir, "release-manifest.json")
		signaturePath = filepath.Join(targetDir, "release-manifest.sigstore.json")
		projection = false
	}
	manifestRaw, err := readLimitedFile(manifestPath, maxManifestBytes)
	if err != nil {
		return nil, fail(CodeTrustUnavailable, "已安装发布清单缺失或不可读")
	}
	bundleRaw, err := readLimitedFile(signaturePath, maxBundleBytes)
	if err != nil {
		return nil, fail(CodeSignatureInvalid, "已安装发布清单签名缺失或不可读")
	}
	releaseKeyID, err := VerifyBlobBundle(bundleRaw, manifestRaw, releaseRing)
	if err != nil {
		return nil, err
	}
	manifest, err := ParseManifest(manifestRaw, DefaultValidationOptions())
	if err != nil {
		return nil, err
	}
	if manifest.ReleaseSequence < policy.MinReleaseSequence {
		return nil, failDetails(CodeRollbackRejected, "已安装发布序列低于信任策略最小值", map[string]any{
			"release_sequence": manifest.ReleaseSequence, "minimum": policy.MinReleaseSequence,
		})
	}
	if manifest.Compatibility.VerifierPolicy != policy.Policy {
		return nil, fail(CodeReleaseInvalid, "已安装发布清单与信任策略的 verifier policy 不一致")
	}
	selected := selectApplications(manifest.Applications, applications)
	if len(selected) == 0 {
		return nil, fail(CodeComponentSetMismatch, "已安装发布清单没有匹配的应用")
	}
	applicationsOut := make([]VerifiedApplication, 0, len(selected))
	for _, application := range selected {
		components := make([]VerifiedComponent, 0, len(application.Components))
		for _, component := range application.Components {
			components = append(components, VerifiedComponent{
				ServiceID: component.ServiceID, Role: component.Role, Conditional: component.Conditional,
				Image: component.Image, RegistrationProtocol: component.RegistrationProtocol,
				CatalogVersion: component.CatalogVersion, Replicas: component.Replicas,
			})
		}
		applicationsOut = append(applicationsOut, VerifiedApplication{
			Code: application.Code, Environment: application.Environment, Components: components,
		})
	}
	images := make([]VerifiedImage, 0, len(manifest.Images))
	for _, image := range manifest.Images {
		images = append(images, VerifiedImage{
			Name: image.Name, Platform: image.Platform, ConfigDigest: image.ConfigDigest,
			ManifestDigest: image.ManifestDigest, IndexDigest: image.IndexDigest,
			Provenance: image.Provenance, ProvenanceBundle: image.ProvenanceBundle,
		})
	}
	profiles := make([]VerifiedProfile, 0, len(manifest.Profiles))
	for _, profile := range manifest.Profiles {
		profiles = append(profiles, VerifiedProfile{
			Path: profile.Path, SHA256: profile.SHA256,
			NormalizedSHA256: profile.NormalizedSHA256, PolicyVersion: profile.PolicyVersion,
		})
	}
	releaseDigest := sha256.Sum256(manifestRaw)
	actualDigest := "sha256:" + hex.EncodeToString(releaseDigest[:])
	if expectedDigest != "" && actualDigest != expectedDigest {
		return nil, failDetails(CodeArtifactMismatch, "已安装发布摘要与期望 release digest 不一致", map[string]any{
			"expected": expectedDigest, "actual": actualDigest,
		})
	}
	if projection {
		if err := verifyInstalledProjection(targetDir, manifest); err != nil {
			return nil, err
		}
		if err := verifyInstalledProvenance(targetDir, manifest, policy, buildRing); err != nil {
			return nil, err
		}
	}
	return &VerifiedReleasePlan{
		ReleaseID: manifest.ReleaseID, ReleaseDigest: actualDigest,
		Channel: manifest.Channel, Sequence: manifest.ReleaseSequence,
		Platform: manifest.TargetPlatform, VerifierPolicy: manifest.Compatibility.VerifierPolicy,
		TrustEpoch: policy.TrustEpoch, ReleaseKeyID: releaseKeyID,
		Profiles: profiles, Images: images, Applications: applicationsOut,
		Status: "INSTALLED_MANIFEST_VERIFIED",
	}, nil
}

func verifyInstalledProvenance(targetDir string, manifest *Manifest, policy *TrustPolicy, buildRing *KeyRing) error {
	for _, image := range manifest.Images {
		if image.Provenance == "" || image.ProvenanceBundle == "" {
			return fail(CodeProvenanceInvalid, "已安装镜像缺少构建证明路径")
		}
		statementPath := filepath.Join(targetDir, "runtime", "release-integrity", filepath.FromSlash(image.Provenance))
		bundlePath := filepath.Join(targetDir, "runtime", "release-integrity", filepath.FromSlash(image.ProvenanceBundle))
		statement, err := readLimitedFile(statementPath, maxStatementBytes)
		if err != nil {
			return fail(CodeProvenanceInvalid, "已安装构建证明缺失或不可读")
		}
		bundle, err := readLimitedFile(bundlePath, maxBundleBytes)
		if err != nil {
			return fail(CodeProvenanceInvalid, "已安装构建证明 bundle 缺失或不可读")
		}
		if _, err := VerifyDSSEBundle(bundle, statement, buildRing); err != nil {
			return err
		}
		parsed, err := ParseStatement(statement)
		if err != nil {
			return err
		}
		if err := validateProvenancePredicate(parsed, image, policy); err != nil {
			return err
		}
	}
	return nil
}

func verifyInstalledProjection(targetDir string, manifest *Manifest) error {
	imagePackages := 0
	for _, artifact := range manifest.Artifacts {
		if artifact.Type != "image-package" {
			continue
		}
		imagePackages++
		path := filepath.Join(targetDir, "packages", filepath.Base(artifact.Path))
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != artifact.Size {
			return failDetails(CodeArtifactMismatch, "安装目标缺少匹配的镜像包或长度不一致", map[string]any{
				"artifact": artifact.Path,
			})
		}
		sum, _, err := hashRegularFile(path, artifact.Size)
		if err != nil || sum != artifact.SHA256 {
			return failDetails(CodeArtifactMismatch, "安装目标镜像包摘要不匹配", map[string]any{"artifact": artifact.Path})
		}
	}
	if imagePackages == 0 {
		return fail(CodeArtifactMismatch, "发布清单没有可核对的镜像包")
	}
	for _, profile := range manifest.Profiles {
		path := filepath.Join(targetDir, "subsystems.d", filepath.Base(profile.Path))
		sum, _, err := hashRegularFile(path, 0)
		if err != nil || sum != profile.SHA256 {
			return failDetails(CodeArtifactMismatch, "安装目标 profile 缺失或摘要不匹配", map[string]any{
				"profile": profile.Path,
			})
		}
	}
	return nil
}

func safeDirectory(value, label string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fail(CodeReleaseInvalid, "%s 无法解析", label)
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fail(CodeReleaseInvalid, "%s 不是安全目录", label)
	}
	return absolute, nil
}

func readLimitedFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("not a regular file")
	}
	if info.Size() <= 0 || info.Size() > limit {
		return nil, fmt.Errorf("file size out of bounds")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) != info.Size() {
		return nil, fmt.Errorf("file changed while reading")
	}
	return raw, nil
}

func verifyArtifacts(mediaDir string, artifacts []Artifact) ([]VerifiedArtifact, map[string]Artifact, error) {
	if len(artifacts) > maxArtifacts {
		return nil, nil, fail(CodeArtifactMismatch, "发布清单 artifact 数量超过上限")
	}
	byPath := make(map[string]Artifact, len(artifacts))
	verified := make([]VerifiedArtifact, 0, len(artifacts))
	var total int64
	for _, artifact := range artifacts {
		if err := safeRelativePath(artifact.Path); err != nil {
			return nil, nil, fail(CodeArtifactMismatch, "artifact 路径不安全：%s", artifact.Path)
		}
		path, err := safeJoin(mediaDir, artifact.Path)
		if err != nil {
			return nil, nil, fail(CodeArtifactMismatch, "artifact 路径逃逸：%s", artifact.Path)
		}
		sum, size, err := hashRegularFile(path, artifact.Size)
		if err != nil {
			return nil, nil, failDetails(CodeArtifactMismatch, "artifact 无法安全读取", map[string]any{
				"path": artifact.Path, "error": err.Error(),
			})
		}
		if size != artifact.Size || sum != artifact.SHA256 {
			return nil, nil, failDetails(CodeArtifactMismatch, "artifact 长度或摘要不匹配", map[string]any{
				"path": artifact.Path, "expected_size": artifact.Size, "actual_size": size,
			})
		}
		total += size
		if total > maxTotalBytes {
			return nil, nil, fail(CodeArtifactMismatch, "artifact 总大小超过上限")
		}
		byPath[artifact.Path] = artifact
		verified = append(verified, VerifiedArtifact{Path: artifact.Path, SHA256: sum, Size: size, Type: artifact.Type})
	}
	sort.Slice(verified, func(i, j int) bool { return verified[i].Path < verified[j].Path })
	return verified, byPath, nil
}

func verifyProfiles(mediaDir string, profiles []Profile) ([]VerifiedProfile, error) {
	verified := make([]VerifiedProfile, 0, len(profiles))
	for _, profile := range profiles {
		path, err := safeJoin(mediaDir, profile.Path)
		if err != nil {
			return nil, fail(CodeArtifactMismatch, "profile 路径逃逸：%s", profile.Path)
		}
		sum, _, err := hashRegularFile(path, 0)
		if err != nil {
			return nil, failDetails(CodeArtifactMismatch, "profile 无法安全读取", map[string]any{
				"path": profile.Path, "error": err.Error(),
			})
		}
		if sum != profile.SHA256 {
			return nil, failDetails(CodeArtifactMismatch, "profile 摘要不匹配", map[string]any{"path": profile.Path})
		}
		verified = append(verified, VerifiedProfile{
			Path: profile.Path, SHA256: profile.SHA256, NormalizedSHA256: profile.NormalizedSHA256,
			PolicyVersion: profile.PolicyVersion,
		})
	}
	sort.Slice(verified, func(i, j int) bool { return verified[i].Path < verified[j].Path })
	return verified, nil
}

func verifyImages(mediaDir string, manifest *Manifest, artifacts map[string]Artifact, policy *TrustPolicy, buildRing *KeyRing, requireProvenance bool) ([]VerifiedImage, []string, error) {
	verified := make([]VerifiedImage, 0, len(manifest.Images))
	keyIDs := make(map[string]bool)
	for _, image := range manifest.Images {
		if image.Package != "" {
			if _, ok := artifacts[image.Package]; !ok {
				return nil, nil, fail(CodeArtifactMismatch, "镜像包未列入 artifacts：%s", image.Package)
			}
		}
		result := VerifiedImage{
			Name: image.Name, Platform: image.Platform, ConfigDigest: image.ConfigDigest,
			ManifestDigest: image.ManifestDigest, IndexDigest: image.IndexDigest,
			Provenance: image.Provenance, ProvenanceBundle: image.ProvenanceBundle,
		}
		if requireProvenance {
			if _, ok := artifacts[image.Provenance]; !ok {
				return nil, nil, fail(CodeProvenanceInvalid, "构建证明未列入 artifacts：%s", image.Name)
			}
			if _, ok := artifacts[image.ProvenanceBundle]; !ok {
				return nil, nil, fail(CodeProvenanceInvalid, "构建证明 bundle 未列入 artifacts：%s", image.Name)
			}
			statementPath, err := safeJoin(mediaDir, image.Provenance)
			if err != nil {
				return nil, nil, fail(CodeProvenanceInvalid, "构建证明路径逃逸：%s", image.Name)
			}
			bundlePath, err := safeJoin(mediaDir, image.ProvenanceBundle)
			if err != nil {
				return nil, nil, fail(CodeProvenanceInvalid, "构建证明 bundle 路径逃逸：%s", image.Name)
			}
			statementRaw, err := readLimitedFile(statementPath, maxStatementBytes)
			if err != nil {
				return nil, nil, fail(CodeProvenanceInvalid, "构建证明不可读：%s", image.Name)
			}
			bundleRaw, err := readLimitedFile(bundlePath, maxBundleBytes)
			if err != nil {
				return nil, nil, fail(CodeProvenanceInvalid, "构建证明 bundle 不可读：%s", image.Name)
			}
			keyID, err := VerifyDSSEBundle(bundleRaw, statementRaw, buildRing)
			if err != nil {
				return nil, nil, err
			}
			statement, err := ParseStatement(statementRaw)
			if err != nil {
				return nil, nil, err
			}
			if err := validateProvenancePredicate(statement, image, policy); err != nil {
				return nil, nil, err
			}
			result.ProvenanceKeyID = keyID
			for _, subject := range statement.Subject {
				for _, digest := range subject.Digest {
					result.Subjects = append(result.Subjects, "sha256:"+digest)
				}
			}
			keyIDs[keyID] = true
		}
		verified = append(verified, result)
	}
	ids := make([]string, 0, len(keyIDs))
	for id := range keyIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return verified, ids, nil
}

type provenancePredicate struct {
	BuildDefinition struct {
		BuildType            string            `json:"buildType"`
		ExternalParameters   json.RawMessage   `json:"externalParameters"`
		ResolvedDependencies []json.RawMessage `json:"resolvedDependencies"`
	} `json:"buildDefinition"`
	RunDetails struct {
		Builder struct {
			ID string `json:"id"`
		} `json:"builder"`
		Metadata struct {
			InvocationID string `json:"invocationId"`
		} `json:"metadata"`
	} `json:"runDetails"`
}

func validateProvenancePredicate(statement *Statement, image Image, policy *TrustPolicy) error {
	var predicate provenancePredicate
	if err := json.Unmarshal(statement.Predicate, &predicate); err != nil {
		return fail(CodeProvenanceInvalid, "SLSA predicate 无法解析")
	}
	if predicate.BuildDefinition.BuildType == "" {
		return fail(CodeProvenanceInvalid, "构建证明缺少 buildType")
	}
	if !allowedValue(policy.AllowedBuildTypes, predicate.BuildDefinition.BuildType) {
		return fail(CodeProvenanceInvalid, "构建类型不在信任策略允许范围内")
	}
	if !allowedValue(policy.AllowedBuilders, predicate.RunDetails.Builder.ID) {
		return fail(CodeProvenanceInvalid, "构建者不在 allowed_builders 中")
	}
	if len(predicate.BuildDefinition.ResolvedDependencies) == 0 {
		return fail(CodeProvenanceInvalid, "构建证明缺少 resolvedDependencies")
	}
	expected := map[string]bool{}
	for _, digest := range image.typedDigests() {
		expected[digest] = true
	}
	matched := false
	for _, subject := range statement.Subject {
		for algorithm, digest := range subject.Digest {
			if algorithm == "sha256" && expected["sha256:"+digest] {
				matched = true
			}
		}
	}
	if !matched {
		return fail(CodeProvenanceInvalid, "构建证明 subject 与镜像摘要不匹配")
	}
	return nil
}

func selectApplications(applications []Application, selected []string) []Application {
	if len(selected) == 0 {
		return applications
	}
	wanted := make(map[string]bool, len(selected))
	for _, app := range selected {
		wanted[app] = true
	}
	out := make([]Application, 0, len(selected))
	for _, application := range applications {
		if wanted[application.Code] {
			out = append(out, application)
			delete(wanted, application.Code)
		}
	}
	if len(wanted) != 0 {
		return nil
	}
	return out
}

func allowedValue(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func safeJoin(root, relative string) (string, error) {
	if err := safeRelativePath(relative); err != nil {
		return "", err
	}
	current := root
	parts := strings.Split(relative, "/")
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("path contains a symlink")
		}
		if index < len(parts)-1 && !info.IsDir() {
			return "", fmt.Errorf("path parent is not a directory")
		}
	}
	return current, nil
}

func hashRegularFile(path string, expectedSize int64) (string, int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", 0, fmt.Errorf("not a regular file")
	}
	if expectedSize > 0 && info.Size() != expectedSize {
		return "", info.Size(), fmt.Errorf("size mismatch")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hasher := sha256.New()
	limit := int64(maxTotalBytes)
	if expectedSize > 0 {
		limit = expectedSize
	}
	written, err := io.Copy(hasher, io.LimitReader(file, limit+1))
	if err != nil {
		return "", written, err
	}
	if expectedSize > 0 && written != expectedSize {
		return "", written, fmt.Errorf("size mismatch")
	}
	return hex.EncodeToString(hasher.Sum(nil)), written, nil
}
