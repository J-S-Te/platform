package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/shared/releaseintegrity"
)

const (
	evidenceSchema = "urn:uip:build-evidence:v1"
	specSchema     = "urn:uip:release-spec:v1"
)

type spec struct {
	Schema            string                         `json:"schema"`
	ReleaseID         string                         `json:"release_id"`
	Channel           string                         `json:"channel"`
	ReleaseSequence   int64                          `json:"release_sequence"`
	BuildInputsSHA256 string                         `json:"build_inputs_sha256"`
	Compatibility     releaseintegrity.Compatibility `json:"compatibility"`
	Applications      []releaseintegrity.Application `json:"applications"`
	Images            []specImage                    `json:"images"`
}

type specImage struct {
	Name          string `json:"name"`
	Tag           string `json:"tag"`
	Package       string `json:"package"`
	ArchiveMember string `json:"archive_member"`
}

type evidence struct {
	Schema               string               `json:"schema"`
	Version              string               `json:"version"`
	TargetPlatform       string               `json:"target_platform"`
	BuildType            string               `json:"build_type"`
	SourceInputSHA256    string               `json:"source_input_sha256"`
	Builder              evidenceBuilder      `json:"builder"`
	Packages             []evidencePackage    `json:"packages"`
	Dockerfiles          []evidenceDockerfile `json:"dockerfiles"`
	SourceIndex          json.RawMessage      `json:"source_index,omitempty"`
	FormalSigningAllowed bool                 `json:"formal_signing_allowed"`
	UnsignedCandidate    bool                 `json:"unsigned_candidate,omitempty"`
	CreatedAt            string               `json:"created_at,omitempty"`
	ToolVersions         map[string]string    `json:"tool_versions,omitempty"`
}

type evidenceBuilder struct {
	ID           string `json:"id"`
	InvocationID string `json:"invocation_id"`
}

type evidencePackage struct {
	Component string          `json:"component"`
	Archive   string          `json:"archive"`
	SHA256    string          `json:"sha256"`
	Size      int64           `json:"size"`
	Images    []evidenceImage `json:"images"`
}

type evidenceImage struct {
	Tag          string `json:"tag"`
	ConfigDigest string `json:"config_digest"`
}

type evidenceDockerfile struct {
	Path              string   `json:"path"`
	SHA256            string   `json:"sha256"`
	From              []string `json:"from"`
	AllBaseRefsPinned bool     `json:"all_base_refs_pinned"`
}

type attestationPlan struct {
	Schema               string          `json:"schema"`
	Candidate            bool            `json:"candidate"`
	FormalSigningAllowed bool            `json:"formal_signing_allowed"`
	BuildType            string          `json:"build_type"`
	BuilderID            string          `json:"builder_id"`
	InvocationID         string          `json:"invocation_id"`
	Statements           []statementPlan `json:"statements"`
}

type statementPlan struct {
	Image     string `json:"image"`
	Statement string `json:"statement"`
	Bundle    string `json:"bundle"`
	Role      string `json:"role"`
}

type releasePlan struct {
	Schema                string `json:"schema"`
	Candidate             bool   `json:"candidate"`
	FormalSigningAllowed  bool   `json:"formal_signing_allowed"`
	Manifest              string `json:"manifest"`
	Signature             string `json:"signature"`
	Role                  string `json:"role"`
	SigningConfigRequired bool   `json:"signing_config_required"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: release-manifest attestations|manifest ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "attestations":
		err = runAttestations(os.Args[2:])
	case "manifest":
		err = runManifest(os.Args[2:])
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "[错误] "+err.Error())
		os.Exit(1)
	}
}

func runAttestations(args []string) error {
	flags := flag.NewFlagSet("attestations", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	media := flags.String("media", "", "read-only release media directory")
	specPath := flags.String("spec", "", "release spec JSON")
	profilesDir := flags.String("profiles-dir", "", "directory with exactly six production profiles")
	planOutput := flags.String("plan-output", "", "write attestation signing plan outside the media")
	candidate := flags.Bool("candidate", false, "allow an unsigned candidate when formal signing is unavailable")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *media == "" || *specPath == "" || *profilesDir == "" || *planOutput == "" {
		return fmt.Errorf("--media, --spec, --profiles-dir and --plan-output are required")
	}
	specValue, evidenceValue, mediaDir, err := loadInputs(*media, *specPath)
	if err != nil {
		return err
	}
	if err := ensureFormal(evidenceValue, *candidate); err != nil {
		return err
	}
	if specValue.BuildInputsSHA256 != evidenceValue.SourceInputSHA256 {
		return fmt.Errorf("spec build_inputs_sha256 与构建证据不一致")
	}
	profiles, err := installProfiles(*profilesDir, mediaDir)
	if err != nil {
		return err
	}
	_ = profiles
	statementsDir := filepath.Join(mediaDir, "attestations")
	if err := os.MkdirAll(statementsDir, 0o700); err != nil {
		return fmt.Errorf("无法创建 attestations 目录")
	}
	plans := make([]statementPlan, 0, len(specValue.Images))
	for _, image := range specValue.Images {
		pkg, evidenceImageValue, err := findImage(evidenceValue, image)
		if err != nil {
			return err
		}
		if err := validateSpecPackage(mediaDir, image, pkg); err != nil {
			return err
		}
		statementPath := filepath.Join("attestations", image.Name+".intoto.json")
		absoluteStatement := filepath.Join(mediaDir, filepath.FromSlash(statementPath))
		if _, err := os.Lstat(absoluteStatement); err == nil {
			return fmt.Errorf("Statement 已存在，拒绝覆盖：%s", statementPath)
		}
		statement, err := buildStatement(image, pkg, evidenceImageValue, evidenceValue)
		if err != nil {
			return err
		}
		if err := writeJSON(absoluteStatement, statement); err != nil {
			return err
		}
		plans = append(plans, statementPlan{
			Image: image.Name, Statement: statementPath,
			Bundle: "attestations/" + image.Name + ".intoto.sigstore.json",
			Role:   "build",
		})
	}
	plan := attestationPlan{
		Schema: "urn:uip:release-attestation-plan:v1", Candidate: *candidate,
		FormalSigningAllowed: evidenceValue.FormalSigningAllowed,
		BuildType:            evidenceValue.BuildType, BuilderID: evidenceValue.Builder.ID,
		InvocationID: evidenceValue.Builder.InvocationID, Statements: plans,
	}
	return writeJSON(*planOutput, plan)
}

func runManifest(args []string) error {
	flags := flag.NewFlagSet("manifest", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	media := flags.String("media", "", "read-only release media directory")
	specPath := flags.String("spec", "", "release spec JSON")
	profilesDir := flags.String("profiles-dir", "", "directory with exactly six production profiles")
	planOutput := flags.String("plan-output", "", "write release signing plan outside the media")
	candidate := flags.Bool("candidate", false, "allow an unsigned candidate when formal signing is unavailable")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *media == "" || *specPath == "" || *profilesDir == "" || *planOutput == "" {
		return fmt.Errorf("--media, --spec, --profiles-dir and --plan-output are required")
	}
	specValue, evidenceValue, mediaDir, err := loadInputs(*media, *specPath)
	if err != nil {
		return err
	}
	if err := ensureFormal(evidenceValue, *candidate); err != nil {
		return err
	}
	if specValue.BuildInputsSHA256 != evidenceValue.SourceInputSHA256 {
		return fmt.Errorf("spec build_inputs_sha256 与构建证据不一致")
	}
	profiles, err := installProfiles(*profilesDir, mediaDir)
	if err != nil {
		return err
	}
	manifestImages := make([]releaseintegrity.Image, 0, len(specValue.Images))
	for _, image := range specValue.Images {
		pkg, evidenceImageValue, err := findImage(evidenceValue, image)
		if err != nil {
			return err
		}
		if err := validateSpecPackage(mediaDir, image, pkg); err != nil {
			return err
		}
		statementPath := filepath.Join("attestations", image.Name+".intoto.json")
		bundlePath := filepath.Join("attestations", image.Name+".intoto.sigstore.json")
		if _, err := os.Lstat(filepath.Join(mediaDir, filepath.FromSlash(statementPath))); err != nil {
			return fmt.Errorf("缺少签名前的 Statement：%s", statementPath)
		}
		if _, err := os.Lstat(filepath.Join(mediaDir, filepath.FromSlash(bundlePath))); err != nil {
			return fmt.Errorf("缺少构建证明 DSSE bundle，请先由构建角色签名：%s", bundlePath)
		}
		manifestImages = append(manifestImages, releaseintegrity.Image{
			Name: image.Name, Platform: evidenceValue.TargetPlatform, Source: "built",
			Package: image.Package, ArchiveMember: image.ArchiveMember,
			ConfigDigest: evidenceImageValue.ConfigDigest,
			Provenance:   statementPath, ProvenanceBundle: bundlePath,
		})
	}
	artifacts, err := collectArtifacts(mediaDir, specValue.Images)
	if err != nil {
		return err
	}
	manifest := releaseintegrity.Manifest{
		Schema:    releaseintegrity.ManifestSchema,
		ReleaseID: specValue.ReleaseID, Channel: specValue.Channel,
		ReleaseSequence:   specValue.ReleaseSequence,
		TargetPlatform:    evidenceValue.TargetPlatform,
		BuildInputsSHA256: specValue.BuildInputsSHA256,
		Compatibility:     specValue.Compatibility,
		Artifacts:         artifacts, Profiles: profiles,
		Images: manifestImages, Applications: specValue.Applications,
	}
	if err := manifest.Validate(releaseintegrity.ValidationOptions{RequireProvenance: true, RequireProd: true}); err != nil {
		return err
	}
	manifestName := "release-manifest.json"
	if *candidate {
		manifestName = "release-manifest.candidate.json"
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := writeBytes(filepath.Join(mediaDir, manifestName), raw); err != nil {
		return err
	}
	plan := releasePlan{
		Schema: "urn:uip:release-signing-plan:v1", Candidate: *candidate,
		FormalSigningAllowed: evidenceValue.FormalSigningAllowed,
		Manifest:             manifestName, Signature: strings.TrimSuffix(manifestName, ".json") + ".sigstore.json",
		Role: "release", SigningConfigRequired: true,
	}
	return writeJSON(*planOutput, plan)
}

func safeMediaPath(mediaDir, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || strings.Contains(relative, "\\") {
		return "", fmt.Errorf("路径必须为相对路径")
	}
	cleaned := filepath.ToSlash(filepath.Clean(relative))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("路径不能逃逸介质目录")
	}
	return filepath.Join(mediaDir, filepath.FromSlash(cleaned)), nil
}

func validateSpecPackage(mediaDir string, image specImage, pkg evidencePackage) error {
	if filepath.Base(image.Package) != pkg.Archive {
		return fmt.Errorf("spec package 与构建证据不一致：%s", image.Name)
	}
	path, err := safeMediaPath(mediaDir, image.Package)
	if err != nil {
		return fmt.Errorf("spec package 路径不安全：%s", image.Name)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("spec package 不存在或不安全：%s", image.Package)
	}
	return nil
}

func loadInputs(media, specPath string) (*spec, *evidence, string, error) {
	mediaDir, err := filepath.Abs(media)
	if err != nil {
		return nil, nil, "", err
	}
	info, err := os.Lstat(mediaDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, "", fmt.Errorf("介质目录不安全")
	}
	var specValue spec
	if err := loadStrictJSON(specPath, &specValue); err != nil {
		return nil, nil, "", fmt.Errorf("发布 spec 不合法：%w", err)
	}
	if specValue.Schema != specSchema {
		return nil, nil, "", fmt.Errorf("发布 spec schema 不支持")
	}
	var evidenceValue evidence
	evidencePath := filepath.Join(mediaDir, "build-evidence.json")
	if err := loadStrictJSON(evidencePath, &evidenceValue); err != nil {
		return nil, nil, "", fmt.Errorf("构建证据不合法：%w", err)
	}
	if evidenceValue.Schema != evidenceSchema {
		return nil, nil, "", fmt.Errorf("构建证据 schema 不支持")
	}
	if evidenceValue.TargetPlatform != releaseintegrity.TargetPlatform {
		return nil, nil, "", fmt.Errorf("构建证据平台不是 linux/amd64")
	}
	if len(specValue.Images) == 0 || len(evidenceValue.Packages) == 0 {
		return nil, nil, "", fmt.Errorf("spec/证据没有镜像")
	}
	for _, pkg := range evidenceValue.Packages {
		path, err := safeMediaPath(mediaDir, pkg.Archive)
		if err != nil {
			return nil, nil, "", fmt.Errorf("构建证据中的镜像包路径不安全：%s", pkg.Archive)
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != pkg.Size {
			return nil, nil, "", fmt.Errorf("构建证据与镜像包长度/类型不一致：%s", pkg.Archive)
		}
		sum, err := hashFile(path)
		if err != nil || sum != pkg.SHA256 {
			return nil, nil, "", fmt.Errorf("构建证据与镜像包摘要不一致：%s", pkg.Archive)
		}
	}
	return &specValue, &evidenceValue, mediaDir, nil
}

func ensureFormal(value *evidence, candidate bool) error {
	if !value.FormalSigningAllowed && !candidate {
		return fmt.Errorf("构建证据 formal_signing_allowed=false；只能使用 --candidate 生成未签候选")
	}
	return nil
}

func findImage(value *evidence, image specImage) (evidencePackage, evidenceImage, error) {
	for _, pkg := range value.Packages {
		for _, candidate := range pkg.Images {
			if candidate.Tag == image.Tag {
				return pkg, candidate, nil
			}
		}
	}
	return evidencePackage{}, evidenceImage{}, fmt.Errorf("spec 镜像标签不在构建证据中：%s", image.Tag)
}

func buildStatement(image specImage, pkg evidencePackage, imageValue evidenceImage, value *evidence) (map[string]any, error) {
	digest := strings.TrimPrefix(imageValue.ConfigDigest, "sha256:")
	if len(digest) != 64 {
		return nil, fmt.Errorf("镜像 config digest 不合法：%s", image.Name)
	}
	packagePath := filepath.Join(filepath.Dir(image.Package), pkg.Archive)
	if filepath.Base(image.Package) != pkg.Archive {
		return nil, fmt.Errorf("spec package 与构建证据不一致：%s", image.Name)
	}
	dependencies := []any{
		map[string]any{"uri": "uip:source-input", "digest": map[string]string{"sha256": value.SourceInputSHA256}},
	}
	for _, dockerfile := range value.Dockerfiles {
		for _, ref := range dockerfile.From {
			dependency := map[string]any{"uri": "pkg:docker/" + ref}
			if index := strings.Index(ref, "@sha256:"); index >= 0 {
				baseDigest := ref[index+len("@sha256:"):]
				if len(baseDigest) == 64 {
					dependency["digest"] = map[string]string{"sha256": baseDigest}
				}
			}
			dependencies = append(dependencies, dependency)
		}
	}
	statement := map[string]any{
		"_type":         "https://in-toto.io/Statement/v1",
		"predicateType": "https://slsa.dev/provenance/v1",
		"subject": []any{map[string]any{
			"name": image.Name, "digest": map[string]string{"sha256": digest},
		}},
		"predicate": map[string]any{
			"buildDefinition": map[string]any{
				"buildType": value.BuildType,
				"externalParameters": map[string]any{
					"platform": value.TargetPlatform, "package": packagePath,
					"package_sha256": pkg.SHA256, "archive_member": image.ArchiveMember,
				},
				"resolvedDependencies": dependencies,
			},
			"runDetails": map[string]any{
				"builder":  map[string]string{"id": value.Builder.ID},
				"metadata": map[string]string{"invocationId": value.Builder.InvocationID},
			},
		},
	}
	return statement, nil
}

func installProfiles(sourceDir, mediaDir string) ([]releaseintegrity.Profile, error) {
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("无法读取 profiles 目录")
	}
	names := make([]string, 0, 6)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		names = append(names, entry.Name())
	}
	if len(names) != 6 {
		return nil, fmt.Errorf("profiles 目录必须恰好包含六份 .yaml，实际 %d", len(names))
	}
	sort.Strings(names)
	targetDir := filepath.Join(mediaDir, "profiles")
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		return nil, fmt.Errorf("无法创建 profiles 目录")
	}
	profiles := make([]releaseintegrity.Profile, 0, 6)
	for _, name := range names {
		source := filepath.Join(sourceDir, name)
		info, err := os.Lstat(source)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("profile 不是安全普通文件：%s", name)
		}
		raw, err := os.ReadFile(source)
		if err != nil {
			return nil, err
		}
		destination := filepath.Join(targetDir, name)
		if existing, err := os.ReadFile(destination); err == nil {
			if string(existing) != string(raw) {
				return nil, fmt.Errorf("已存在不同的 profile：%s", name)
			}
		} else if os.IsNotExist(err) {
			if err := writeBytes(destination, raw); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
		sum := sha256.Sum256(raw)
		profiles = append(profiles, releaseintegrity.Profile{
			Path: "profiles/" + name, SHA256: hex.EncodeToString(sum[:]),
			NormalizedSHA256: hex.EncodeToString(sum[:]), PolicyVersion: 1,
		})
	}
	return profiles, nil
}

func collectArtifacts(mediaDir string, images []specImage) ([]releaseintegrity.Artifact, error) {
	skip := map[string]bool{
		"release-manifest.json": true, "release-manifest.candidate.json": true,
		"release-manifest.sigstore.json": true, "release-signing-plan.json": true,
		"attestation-signing-plan.json": true,
	}
	artifacts := []releaseintegrity.Artifact{}
	err := filepath.WalkDir(mediaDir, func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if current == mediaDir {
			return nil
		}
		relative, err := filepath.Rel(mediaDir, current)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			return nil
		}
		if skip[relative] {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("介质包含非普通文件：%s", relative)
		}
		sum, err := hashFile(current)
		if err != nil {
			return err
		}
		artifacts = append(artifacts, releaseintegrity.Artifact{
			Path: relative, SHA256: sum, Size: info.Size(), Type: artifactType(relative),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
	return artifacts, nil
}

func artifactType(relative string) string {
	base := path.Base(relative)
	switch {
	case strings.HasSuffix(relative, ".intoto.json"), strings.HasSuffix(relative, ".intoto.sigstore.json"):
		return "provenance"
	case base == "SHA256SUMS", strings.HasSuffix(base, ".sha256"):
		return "checksum"
	case base == "build-evidence.json":
		return "provenance"
	case strings.HasSuffix(base, ".tar.gz"):
		if strings.HasPrefix(base, "host-dependencies-") {
			return "host-dependencies"
		}
		if strings.HasSuffix(base, "-linux-amd64.tar.gz") {
			return "image-package"
		}
		return "asset"
	case strings.HasSuffix(base, ".sh"), strings.HasSuffix(base, ".py"):
		return "script"
	case strings.HasPrefix(relative, "profiles/"):
		return "template"
	default:
		return "other"
	}
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func loadStrictJSON(path string, target any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("存在尾随 JSON")
		}
		return err
	}
	return nil
}

func writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return writeBytes(path, raw)
}

func writeBytes(path string, raw []byte) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("输出路径必须是绝对路径")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("输出路径不是安全普通文件")
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".release-manifest.")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(raw); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}
