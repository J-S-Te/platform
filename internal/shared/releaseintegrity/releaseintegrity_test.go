package releaseintegrity

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fixtureKey struct {
	id      string
	private *ecdsa.PrivateKey
	public  string
}

func newFixtureKey(t *testing.T, id string) fixtureKey {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return fixtureKey{
		id:      id,
		private: private,
		public:  string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
	}
}

type fixture struct {
	media       string
	trust       string
	manifest    Manifest
	manifestRaw []byte
	release     fixtureKey
	build       fixtureKey
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	media := t.TempDir()
	trust := t.TempDir()
	if err := os.MkdirAll(filepath.Join(media, "profiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(media, "packages"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(media, "attestations"), 0o755); err != nil {
		t.Fatal(err)
	}

	write := func(relative string, data []byte) string {
		t.Helper()
		path := filepath.Join(media, filepath.FromSlash(relative))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		return hexString(sum[:])
	}

	release := newFixtureKey(t, "release-key")
	build := newFixtureKey(t, "build-key")
	payloadHash := write("payload.bin", []byte("release payload"))
	packageHash := write("packages/platform.tar.gz", []byte("immutable image package"))
	configDigest := strings.Repeat("a", 64)
	statement := []byte(`{
  "_type": "https://in-toto.io/Statement/v1",
  "predicateType": "https://slsa.dev/provenance/v1",
  "subject": [{"name": "platform", "digest": {"sha256": "` + configDigest + `"}}],
  "predicate": {
    "buildDefinition": {
      "buildType": "https://uip.dev/buildtypes/container-v1",
      "externalParameters": {"platform": "linux/amd64"},
      "resolvedDependencies": [{"uri": "git+https://example.invalid/uip", "digest": {"sha256": "` + strings.Repeat("b", 64) + `"}}]
    },
    "runDetails": {"builder": {"id": "builder://uip/release"}, "metadata": {"invocationId": "inv-1"}}
  }
}`)
	statementHash := write("attestations/platform.intoto.json", statement)
	statementBundle, err := signDSSE(t, build, statement)
	if err != nil {
		t.Fatal(err)
	}
	statementBundleHash := write("attestations/platform.intoto.sigstore.json", statementBundle)

	profiles := make([]Profile, 0, 6)
	artifacts := []Artifact{
		{Path: "payload.bin", SHA256: payloadHash, Size: int64(len("release payload")), Type: "asset"},
		{Path: "packages/platform.tar.gz", SHA256: packageHash, Size: int64(len("immutable image package")), Type: "image-package"},
		{Path: "attestations/platform.intoto.json", SHA256: statementHash, Size: int64(len(statement)), Type: "provenance"},
		{Path: "attestations/platform.intoto.sigstore.json", SHA256: statementBundleHash, Size: int64(len(statementBundle)), Type: "provenance"},
	}
	for index := 0; index < 6; index++ {
		relative := "profiles/" + string(rune('a'+index)) + ".yaml"
		data := []byte("profile: " + relative + "\n")
		sum := write(relative, data)
		profiles = append(profiles, Profile{
			Path: relative, SHA256: sum, NormalizedSHA256: strings.Repeat("c", 64), PolicyVersion: 1,
		})
		artifacts = append(artifacts, Artifact{Path: relative, SHA256: sum, Size: int64(len(data)), Type: "template"})
	}

	manifest := Manifest{
		Schema:            ManifestSchema,
		ReleaseID:         "rel-20261022-1",
		Channel:           "production",
		ReleaseSequence:   2,
		TargetPlatform:    TargetPlatform,
		BuildInputsSHA256: strings.Repeat("d", 64),
		Compatibility: Compatibility{
			VerifierPolicy:           VendorKeyOfflinePolicy,
			MinAPIReleaseIntegrity:   "release-integrity-v1",
			MinAgentReleaseIntegrity: "release-integrity-v1",
			CatalogVersion:           1,
			RegistrationProtocols:    map[string]int{"data_analysis": 2},
			SharedControlPlaneSHA256: strings.Repeat("e", 64),
		},
		Artifacts: artifacts,
		Profiles:  profiles,
		Images: []Image{{
			Name: "platform", Platform: TargetPlatform, Source: "built",
			Package: "packages/platform.tar.gz", ArchiveMember: "images.tar",
			ConfigDigest:     "sha256:" + configDigest,
			Provenance:       "attestations/platform.intoto.json",
			ProvenanceBundle: "attestations/platform.intoto.sigstore.json",
		}},
		Applications: []Application{{
			Code: "data_analysis", Environment: "prod",
			Components: []Component{{
				ServiceID: "data-analysis-api", Role: "api", Image: "platform",
				RegistrationProtocol: 2, CatalogVersion: 1, Replicas: 1,
			}},
		}},
	}
	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestBundle, err := signBlob(t, release, manifestRaw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(media, "release-manifest.json"), manifestRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(media, "release-manifest.sigstore.json"), manifestBundle, 0o600); err != nil {
		t.Fatal(err)
	}

	policy := TrustPolicy{
		Schema: TrustSchema, Policy: VendorKeyOfflinePolicy, TrustEpoch: 1,
		ReleaseKeys:        []TrustKey{{ID: release.id, Algorithm: "ecdsa-p256", PublicKeyPEM: release.public}},
		BuildKeys:          []TrustKey{{ID: build.id, Algorithm: "ecdsa-p256", PublicKeyPEM: build.public}},
		AllowedBuilders:    []string{"builder://uip/release"},
		AllowedBuildTypes:  []string{"https://uip.dev/buildtypes/container-v1"},
		MinReleaseSequence: 1,
	}
	policyRaw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trust, "trust-policy.json"), policyRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	return &fixture{media: media, trust: trust, manifest: manifest, manifestRaw: manifestRaw, release: release, build: build}
}

func signBlob(t *testing.T, key fixtureKey, payload []byte) ([]byte, error) {
	t.Helper()
	digest := sha256.Sum256(payload)
	signature, err := ecdsa.SignASN1(rand.Reader, key.private, digest[:])
	if err != nil {
		return nil, err
	}
	bundle := map[string]any{
		"mediaType":            "application/vnd.dev.sigstore.bundle.v0.3+json",
		"verificationMaterial": map[string]any{},
		"messageSignature": map[string]any{
			"messageDigest": map[string]any{"algorithm": "SHA2_256", "digest": base64.StdEncoding.EncodeToString(digest[:])},
			"signature":     base64.StdEncoding.EncodeToString(signature),
		},
	}
	return json.Marshal(bundle)
}

func signDSSE(t *testing.T, key fixtureKey, statement []byte) ([]byte, error) {
	t.Helper()
	payloadType := "application/vnd.in-toto+json"
	pae := dssePAE(payloadType, statement)
	digest := sha256.Sum256(pae)
	signature, err := ecdsa.SignASN1(rand.Reader, key.private, digest[:])
	if err != nil {
		return nil, err
	}
	bundle := map[string]any{
		"mediaType":            "application/vnd.dev.sigstore.bundle.v0.3+json",
		"verificationMaterial": map[string]any{},
		"dsseEnvelope": map[string]any{
			"payloadType": payloadType,
			"payload":     base64.StdEncoding.EncodeToString(statement),
			"signatures":  []map[string]any{{"keyid": key.id, "sig": base64.StdEncoding.EncodeToString(signature)}},
		},
	}
	return json.Marshal(bundle)
}

func hexString(raw []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(raw)*2)
	for index, value := range raw {
		out[index*2] = digits[value>>4]
		out[index*2+1] = digits[value&0x0f]
	}
	return string(out)
}

func TestVerifyMediaValid(t *testing.T) {
	fixture := newFixture(t)
	plan, err := VerifyMedia(VerifyOptions{
		MediaDir: fixture.media, TrustDir: fixture.trust,
		Applications: []string{"data_analysis"}, RequireProvenance: true,
	})
	if err != nil {
		t.Fatalf("VerifyMedia: %v", err)
	}
	if plan.Status != "VERIFIED" || plan.ReleaseID != fixture.manifest.ReleaseID {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if plan.ReleaseKeyID != fixture.release.id {
		t.Fatalf("release key id = %q", plan.ReleaseKeyID)
	}
	if len(plan.Profiles) != 6 || len(plan.Images) != 1 || len(plan.Applications) != 1 {
		t.Fatalf("unexpected plan shape: %+v", plan)
	}
	if plan.Images[0].ProvenanceKeyID != fixture.build.id {
		t.Fatalf("provenance key id = %q", plan.Images[0].ProvenanceKeyID)
	}
}

func TestVerifyMediaRejectsTamperedManifest(t *testing.T) {
	fixture := newFixture(t)
	path := filepath.Join(fixture.media, "release-manifest.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] = raw[0] ^ 0x01
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = VerifyMedia(VerifyOptions{MediaDir: fixture.media, TrustDir: fixture.trust, RequireProvenance: true})
	if err == nil || !strings.Contains(err.Error(), "RELEASE_SIGNATURE_INVALID") {
		t.Fatalf("expected signature failure, got %v", err)
	}
}

func TestVerifyMediaRejectsWrongReleaseKey(t *testing.T) {
	fixture := newFixture(t)
	other := newFixtureKey(t, "other-release")
	var policy TrustPolicy
	raw, err := os.ReadFile(filepath.Join(fixture.trust, "trust-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &policy); err != nil {
		t.Fatal(err)
	}
	policy.ReleaseKeys = []TrustKey{{ID: other.id, Algorithm: "ecdsa-p256", PublicKeyPEM: other.public}}
	updated, _ := json.Marshal(policy)
	if err := os.WriteFile(filepath.Join(fixture.trust, "trust-policy.json"), updated, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = VerifyMedia(VerifyOptions{MediaDir: fixture.media, TrustDir: fixture.trust, RequireProvenance: true})
	if err == nil || !strings.Contains(err.Error(), "RELEASE_SIGNATURE_INVALID") {
		t.Fatalf("expected wrong-key failure, got %v", err)
	}
}

func TestVerifyMediaRejectsTamperedArtifact(t *testing.T) {
	fixture := newFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.media, "payload.bin"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := VerifyMedia(VerifyOptions{MediaDir: fixture.media, TrustDir: fixture.trust, RequireProvenance: true})
	if err == nil || !strings.Contains(err.Error(), "RELEASE_ARTIFACT_MISMATCH") {
		t.Fatalf("expected artifact failure, got %v", err)
	}
}

func TestVerifyMediaRejectsPathTraversal(t *testing.T) {
	fixture := newFixture(t)
	fixture.manifest.Artifacts[0].Path = "../payload.bin"
	raw, _ := json.Marshal(fixture.manifest)
	bundle, _ := signBlob(t, fixture.release, raw)
	if err := os.WriteFile(filepath.Join(fixture.media, "release-manifest.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.media, "release-manifest.sigstore.json"), bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := VerifyMedia(VerifyOptions{MediaDir: fixture.media, TrustDir: fixture.trust, RequireProvenance: true})
	if err == nil || !strings.Contains(err.Error(), "RELEASE_INVALID") {
		t.Fatalf("expected path validation failure, got %v", err)
	}
}

func TestVerifyMediaRejectsDuplicateManifestKey(t *testing.T) {
	fixture := newFixture(t)
	raw := bytes.Replace(fixture.manifestRaw, []byte(`"release_id":"rel-20261022-1"`), []byte(`"release_id":"rel-20261022-1","release_id":"rel-20261022-1"`), 1)
	if bytes.Equal(raw, fixture.manifestRaw) {
		t.Fatal("fixture manifest did not contain release_id")
	}
	bundle, _ := signBlob(t, fixture.release, raw)
	if err := os.WriteFile(filepath.Join(fixture.media, "release-manifest.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.media, "release-manifest.sigstore.json"), bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := VerifyMedia(VerifyOptions{MediaDir: fixture.media, TrustDir: fixture.trust, RequireProvenance: true})
	if err == nil || !strings.Contains(err.Error(), "RELEASE_INVALID") {
		t.Fatalf("expected duplicate-key failure, got %v", err)
	}
}

func TestVerifyMediaRejectsPlatformMismatch(t *testing.T) {
	fixture := newFixture(t)
	fixture.manifest.TargetPlatform = "linux/arm64"
	raw, _ := json.Marshal(fixture.manifest)
	bundle, _ := signBlob(t, fixture.release, raw)
	if err := os.WriteFile(filepath.Join(fixture.media, "release-manifest.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.media, "release-manifest.sigstore.json"), bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := VerifyMedia(VerifyOptions{MediaDir: fixture.media, TrustDir: fixture.trust, RequireProvenance: true})
	if err == nil || !strings.Contains(err.Error(), "RELEASE_PLATFORM_MISMATCH") {
		t.Fatalf("expected platform failure, got %v", err)
	}
}

func TestVerifyMediaRejectsMissingProvenance(t *testing.T) {
	fixture := newFixture(t)
	fixture.manifest.Images[0].Provenance = ""
	fixture.manifest.Images[0].ProvenanceBundle = ""
	raw, _ := json.Marshal(fixture.manifest)
	bundle, _ := signBlob(t, fixture.release, raw)
	if err := os.WriteFile(filepath.Join(fixture.media, "release-manifest.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.media, "release-manifest.sigstore.json"), bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := VerifyMedia(VerifyOptions{MediaDir: fixture.media, TrustDir: fixture.trust, RequireProvenance: true})
	if err == nil || !strings.Contains(err.Error(), "RELEASE_PROVENANCE_INVALID") {
		t.Fatalf("expected provenance failure, got %v", err)
	}
}

func TestVerifyMediaRejectsTlogMaterial(t *testing.T) {
	fixture := newFixture(t)
	bundle, _ := signBlob(t, fixture.release, fixture.manifestRaw)
	var decoded map[string]any
	if err := json.Unmarshal(bundle, &decoded); err != nil {
		t.Fatal(err)
	}
	decoded["verificationMaterial"] = map[string]any{"tlogEntries": []any{map[string]any{"logIndex": "1"}}}
	bundle, _ = json.Marshal(decoded)
	if err := os.WriteFile(filepath.Join(fixture.media, "release-manifest.sigstore.json"), bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := VerifyMedia(VerifyOptions{MediaDir: fixture.media, TrustDir: fixture.trust, RequireProvenance: true})
	if err == nil || !strings.Contains(err.Error(), "RELEASE_TRUST_UNAVAILABLE") {
		t.Fatalf("expected tlog rejection, got %v", err)
	}
}

func TestVerifyMediaRejectsDSSESubjectMismatch(t *testing.T) {
	fixture := newFixture(t)
	statement := []byte(`{
  "_type": "https://in-toto.io/Statement/v1",
  "predicateType": "https://slsa.dev/provenance/v1",
  "subject": [{"name": "platform", "digest": {"sha256": "` + strings.Repeat("f", 64) + `"}}],
  "predicate": {
    "buildDefinition": {"buildType": "https://uip.dev/buildtypes/container-v1", "externalParameters": {}, "resolvedDependencies": [{"uri": "x", "digest": {"sha256": "` + strings.Repeat("b", 64) + `"}}]},
    "runDetails": {"builder": {"id": "builder://uip/release"}, "metadata": {"invocationId": "inv-1"}}
  }
}`)
	bundle, _ := signDSSE(t, fixture.build, statement)
	if err := os.WriteFile(filepath.Join(fixture.media, "attestations/platform.intoto.json"), statement, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.media, "attestations/platform.intoto.sigstore.json"), bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	// 清单仍包含旧产物哈希；用新的声明哈希
	// 重新签名，使 DSSE 主体检查成为触发失败的门禁。
	statementSum := sha256.Sum256(statement)
	bundleSum := sha256.Sum256(bundle)
	for index := range fixture.manifest.Artifacts {
		switch fixture.manifest.Artifacts[index].Path {
		case "attestations/platform.intoto.json":
			fixture.manifest.Artifacts[index].SHA256 = hexString(statementSum[:])
			fixture.manifest.Artifacts[index].Size = int64(len(statement))
		case "attestations/platform.intoto.sigstore.json":
			fixture.manifest.Artifacts[index].SHA256 = hexString(bundleSum[:])
			fixture.manifest.Artifacts[index].Size = int64(len(bundle))
		}
	}
	raw, _ := json.Marshal(fixture.manifest)
	manifestBundle, _ := signBlob(t, fixture.release, raw)
	if err := os.WriteFile(filepath.Join(fixture.media, "release-manifest.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.media, "release-manifest.sigstore.json"), manifestBundle, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := VerifyMedia(VerifyOptions{MediaDir: fixture.media, TrustDir: fixture.trust, RequireProvenance: true})
	if err == nil || !strings.Contains(err.Error(), "RELEASE_PROVENANCE_INVALID") {
		t.Fatalf("expected subject mismatch, got %v", err)
	}
}

func TestVerifyMediaRejectsWrongBuildKey(t *testing.T) {
	fixture := newFixture(t)
	other := newFixtureKey(t, "other-build")
	var policy TrustPolicy
	raw, err := os.ReadFile(filepath.Join(fixture.trust, "trust-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &policy); err != nil {
		t.Fatal(err)
	}
	policy.BuildKeys = []TrustKey{{ID: other.id, Algorithm: "ecdsa-p256", PublicKeyPEM: other.public}}
	updated, _ := json.Marshal(policy)
	if err := os.WriteFile(filepath.Join(fixture.trust, "trust-policy.json"), updated, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = VerifyMedia(VerifyOptions{MediaDir: fixture.media, TrustDir: fixture.trust, RequireProvenance: true})
	if err == nil || !strings.Contains(err.Error(), "RELEASE_PROVENANCE_INVALID") {
		t.Fatalf("expected wrong build key failure, got %v", err)
	}
}

func TestVerifyMediaRejectsUnknownApplication(t *testing.T) {
	fixture := newFixture(t)
	_, err := VerifyMedia(VerifyOptions{
		MediaDir: fixture.media, TrustDir: fixture.trust,
		Applications: []string{"does_not_exist"}, RequireProvenance: true,
	})
	if err == nil || !strings.Contains(err.Error(), "RELEASE_COMPONENT_SET_MISMATCH") {
		t.Fatalf("expected unknown application failure, got %v", err)
	}
}

func TestVerifyMediaRejectsBundleAmbiguity(t *testing.T) {
	fixture := newFixture(t)
	bundle, _ := signBlob(t, fixture.release, fixture.manifestRaw)
	var decoded map[string]any
	if err := json.Unmarshal(bundle, &decoded); err != nil {
		t.Fatal(err)
	}
	decoded["dsseEnvelope"] = map[string]any{
		"payloadType": "application/vnd.in-toto+json",
		"payload":     base64.StdEncoding.EncodeToString([]byte(`{}`)),
		"signatures":  []any{},
	}
	bundle, _ = json.Marshal(decoded)
	if err := os.WriteFile(filepath.Join(fixture.media, "release-manifest.sigstore.json"), bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := VerifyMedia(VerifyOptions{MediaDir: fixture.media, TrustDir: fixture.trust, RequireProvenance: true})
	if err == nil || !strings.Contains(err.Error(), "RELEASE_SIGNATURE_INVALID") {
		t.Fatalf("expected ambiguous bundle failure, got %v", err)
	}
}

func TestCheckInstalledVerifiesManifestCopy(t *testing.T) {
	fixture := newFixture(t)
	plan, err := CheckInstalled(fixture.media, fixture.trust, []string{"data_analysis"})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	if plan.Status != "INSTALLED_MANIFEST_VERIFIED" {
		t.Fatalf("status = %q", plan.Status)
	}
}

func TestLoadTrustPolicyRejectsMissingBuildTypes(t *testing.T) {
	dir := t.TempDir()
	key := newFixtureKey(t, "release-key")
	policy := TrustPolicy{
		Schema: TrustSchema, Policy: VendorKeyOfflinePolicy, TrustEpoch: 1,
		ReleaseKeys:     []TrustKey{{ID: key.id, Algorithm: "ecdsa-p256", PublicKeyPEM: key.public}},
		BuildKeys:       []TrustKey{{ID: "build-key", Algorithm: "ecdsa-p256", PublicKeyPEM: key.public}},
		AllowedBuilders: []string{"builder://uip/release"},
	}
	raw, _ := json.Marshal(policy)
	if err := os.WriteFile(filepath.Join(dir, "trust-policy.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := LoadTrustPolicy(dir); err == nil || !strings.Contains(err.Error(), "RELEASE_TRUST_UNAVAILABLE") {
		t.Fatalf("expected trust failure, got %v", err)
	}
}

func installedSnapshot(t *testing.T, fixture *fixture) string {
	t.Helper()
	target := t.TempDir()
	destination := filepath.Join(target, "runtime", "release-integrity")
	if err := os.MkdirAll(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"release-manifest.json", "release-manifest.sigstore.json"} {
		raw, err := os.ReadFile(filepath.Join(fixture.media, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(destination, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	packages := filepath.Join(target, "packages")
	profiles := filepath.Join(target, "subsystems.d")
	if err := os.MkdirAll(packages, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(profiles, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range fixture.manifest.Artifacts {
		if artifact.Type != "image-package" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(fixture.media, filepath.FromSlash(artifact.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(packages, filepath.Base(artifact.Path)), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, profile := range fixture.manifest.Profiles {
		raw, err := os.ReadFile(filepath.Join(fixture.media, filepath.FromSlash(profile.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(profiles, filepath.Base(profile.Path)), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, image := range fixture.manifest.Images {
		for _, relative := range []string{image.Provenance, image.ProvenanceBundle} {
			source := filepath.Join(fixture.media, filepath.FromSlash(relative))
			raw, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(target, "runtime", "release-integrity", filepath.FromSlash(relative))
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(destination, raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return target
}

func TestCheckInstalledVerifiesRuntimeSnapshot(t *testing.T) {
	fixture := newFixture(t)
	target := installedSnapshot(t, fixture)
	plan, err := CheckInstalled(target, fixture.trust, []string{"data_analysis"})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	if plan.Status != "INSTALLED_MANIFEST_VERIFIED" || plan.ReleaseID != fixture.manifest.ReleaseID {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if len(plan.Artifacts) != 0 {
		t.Fatalf("installed manifest verification must not claim medium artifacts were rehashed: %d", len(plan.Artifacts))
	}
}

func TestCheckInstalledRejectsTamperedSnapshot(t *testing.T) {
	fixture := newFixture(t)
	target := installedSnapshot(t, fixture)
	path := filepath.Join(target, "runtime", "release-integrity", "release-manifest.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 0x01
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckInstalled(target, fixture.trust, nil); err == nil || !strings.Contains(err.Error(), "RELEASE_SIGNATURE_INVALID") {
		t.Fatalf("expected signature failure, got %v", err)
	}
}

func TestCheckInstalledRejectsMissingSignature(t *testing.T) {
	fixture := newFixture(t)
	target := installedSnapshot(t, fixture)
	if err := os.Remove(filepath.Join(target, "runtime", "release-integrity", "release-manifest.sigstore.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckInstalled(target, fixture.trust, nil); err == nil || !strings.Contains(err.Error(), "RELEASE_SIGNATURE_INVALID") {
		t.Fatalf("expected missing signature failure, got %v", err)
	}
}

func TestCheckInstalledRejectsUnknownApplication(t *testing.T) {
	fixture := newFixture(t)
	target := installedSnapshot(t, fixture)
	if _, err := CheckInstalled(target, fixture.trust, []string{"unknown_app"}); err == nil || !strings.Contains(err.Error(), "RELEASE_COMPONENT_SET_MISMATCH") {
		t.Fatalf("expected application mismatch, got %v", err)
	}
}

func TestCheckInstalledRejectsRollbackSequence(t *testing.T) {
	fixture := newFixture(t)
	target := installedSnapshot(t, fixture)
	var policy TrustPolicy
	raw, err := os.ReadFile(filepath.Join(fixture.trust, "trust-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &policy); err != nil {
		t.Fatal(err)
	}
	policy.MinReleaseSequence = 10
	updated, _ := json.Marshal(policy)
	if err := os.WriteFile(filepath.Join(fixture.trust, "trust-policy.json"), updated, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckInstalled(target, fixture.trust, nil); err == nil || !strings.Contains(err.Error(), "RELEASE_ROLLBACK_REJECTED") {
		t.Fatalf("expected rollback rejection, got %v", err)
	}
}

func TestCheckInstalledBindsExpectedReleaseDigest(t *testing.T) {
	fixture := newFixture(t)
	target := installedSnapshot(t, fixture)
	plan, err := CheckInstalled(target, fixture.trust, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CheckInstalledWithExpected(target, fixture.trust, nil, plan.ReleaseDigest); err != nil {
		t.Fatalf("matching expected digest: %v", err)
	}
	wrong := "sha256:" + strings.Repeat("f", 64)
	if _, err := CheckInstalledWithExpected(target, fixture.trust, nil, wrong); err == nil || !strings.Contains(err.Error(), "RELEASE_ARTIFACT_MISMATCH") {
		t.Fatalf("expected digest mismatch, got %v", err)
	}
	if _, err := CheckInstalledWithExpected(target, fixture.trust, nil, "not-a-digest"); err == nil || !strings.Contains(err.Error(), "RELEASE_INVALID") {
		t.Fatalf("expected malformed digest rejection, got %v", err)
	}
}

func TestCheckInstalledRejectsTamperedInstalledPackage(t *testing.T) {
	fixture := newFixture(t)
	target := installedSnapshot(t, fixture)
	path := filepath.Join(target, "packages", "platform.tar.gz")
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckInstalled(target, fixture.trust, nil); err == nil || !strings.Contains(err.Error(), "RELEASE_ARTIFACT_MISMATCH") {
		t.Fatalf("expected installed package mismatch, got %v", err)
	}
}

func TestCheckInstalledRejectsTamperedInstalledProfile(t *testing.T) {
	fixture := newFixture(t)
	target := installedSnapshot(t, fixture)
	path := filepath.Join(target, "subsystems.d", "a.yaml")
	if err := os.WriteFile(path, []byte("profile: tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckInstalled(target, fixture.trust, nil); err == nil || !strings.Contains(err.Error(), "RELEASE_ARTIFACT_MISMATCH") {
		t.Fatalf("expected installed profile mismatch, got %v", err)
	}
}

func TestCheckInstalledRejectsTamperedAttestation(t *testing.T) {
	fixture := newFixture(t)
	target := installedSnapshot(t, fixture)
	path := filepath.Join(target, "runtime", "release-integrity", "attestations", "platform.intoto.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 0x01
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckInstalled(target, fixture.trust, nil); err == nil || !strings.Contains(err.Error(), "RELEASE_PROVENANCE_INVALID") {
		t.Fatalf("expected provenance failure, got %v", err)
	}
}
