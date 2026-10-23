package main

import (
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
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/shared/releaseintegrity"
)

type testKey struct {
	private *ecdsa.PrivateKey
	public  string
}

func newTestKey(t *testing.T) testKey {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return testKey{private: private, public: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}
}

func signBlob(t *testing.T, key testKey, payload []byte) []byte {
	t.Helper()
	digest := sha256.Sum256(payload)
	signature, err := ecdsa.SignASN1(rand.Reader, key.private, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := json.Marshal(map[string]any{
		"mediaType":            "application/vnd.dev.sigstore.bundle.v0.3+json",
		"verificationMaterial": map[string]any{},
		"messageSignature": map[string]any{
			"messageDigest": map[string]string{"algorithm": "SHA2_256", "digest": base64.StdEncoding.EncodeToString(digest[:])},
			"signature":     base64.StdEncoding.EncodeToString(signature),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func signDSSE(t *testing.T, key testKey, statement []byte) []byte {
	t.Helper()
	payloadType := "application/vnd.in-toto+json"
	pae := []byte("DSSEv1 " + itoa(len(payloadType)) + " " + payloadType + " " + itoa(len(statement)) + " " + string(statement))
	digest := sha256.Sum256(pae)
	signature, err := ecdsa.SignASN1(rand.Reader, key.private, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := json.Marshal(map[string]any{
		"mediaType":            "application/vnd.dev.sigstore.bundle.v0.3+json",
		"verificationMaterial": map[string]any{},
		"dsseEnvelope": map[string]any{
			"payloadType": payloadType,
			"payload":     base64.StdEncoding.EncodeToString(statement),
			"signatures":  []map[string]string{{"keyid": "test-build", "sig": base64.StdEncoding.EncodeToString(signature)}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func writeTestFile(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateManifestAndVerify(t *testing.T) {
	root := t.TempDir()
	media := filepath.Join(root, "media")
	profilesDir := filepath.Join(root, "profiles-src")
	workspace := filepath.Join(root, "workspace")
	for _, dir := range []string{media, profilesDir, workspace, filepath.Join(workspace, "platform")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configDigest := "sha256:" + "a" + repeat("0", 63)
	packageName := "platform-backend-test-linux-amd64.tar.gz"
	packagePath := filepath.Join(media, packageName)
	packageEnv := "PACKAGE_FORMAT=2\nCOMPONENT=platform\nVERSION=test\nPLATFORM=linux/amd64\nIMAGES=uip-package/platform:test\nIMAGE_CONFIG_DIGESTS=" + configDigest + "\n"
	archive := makeTarGz(t, packagePath, "package.env", []byte(packageEnv))
	digest := sha256.Sum256(archive)
	writeTestFile(t, packagePath+".sha256", []byte(hexString(digest[:])+"  "+packageName+"\n"))
	writeTestFile(t, filepath.Join(media, "BUILD_INFO.txt"), []byte("RELEASE_VERSION=test\nTARGET_PLATFORM=linux/amd64\nDOCKER_CLIENT_VERSION=29\nDOCKER_SERVER_VERSION=29\nBUILDX_VERSION=buildx\n"))
	dockerfile := filepath.Join(workspace, "platform", "Dockerfile")
	writeTestFile(t, dockerfile, []byte("FROM alpine@sha256:"+repeat("b", 64)+"\n"))
	dockerfileDigest := sha256.Sum256([]byte("FROM alpine@sha256:" + repeat("b", 64) + "\n"))
	evidence := map[string]any{
		"schema": "urn:uip:build-evidence:v1", "version": "test", "target_platform": "linux/amd64",
		"build_type":          "https://uip.dev/buildtypes/container-v1",
		"source_input_sha256": repeat("c", 64),
		"builder":             map[string]string{"id": "builder://uip/test", "invocation_id": "inv-1"},
		"packages": []any{map[string]any{
			"component": "platform", "archive": packageName,
			"sha256": hexString(digest[:]), "size": len(archive),
			"images": []any{map[string]string{"tag": "uip-package/platform:test", "config_digest": configDigest}},
		}},
		"dockerfiles": []any{map[string]any{
			"path": "platform/Dockerfile", "sha256": hexString(dockerfileDigest[:]),
			"from": []string{"alpine@sha256:" + repeat("b", 64)}, "all_base_refs_pinned": true,
		}},
		"source_index":           map[string]any{"path": "source-input-index.tsv", "sha256": repeat("e", 64), "entries": 1},
		"formal_signing_allowed": true,
		"unsigned_candidate":     false,
		"created_at":             "2026-10-22T00:00:00Z",
		"tool_versions":          map[string]string{"docker": "test", "buildx": "test"},
	}
	evidenceRaw, _ := json.Marshal(evidence)
	writeTestFile(t, filepath.Join(media, "build-evidence.json"), evidenceRaw)
	for index := 0; index < 6; index++ {
		writeTestFile(t, filepath.Join(profilesDir, string(rune('a'+index))+".yaml"), []byte("profile: "+string(rune('a'+index))+"\n"))
	}
	specValue := map[string]any{
		"schema": specSchema, "release_id": "test", "channel": "production", "release_sequence": 1,
		"build_inputs_sha256": repeat("c", 64),
		"compatibility": map[string]any{
			"verifier_policy": "vendor-key-offline-v1", "min_api_release_integrity": "release-integrity-v1",
			"min_agent_release_integrity": "release-integrity-v1", "catalog_version": 1,
			"registration_protocols":      map[string]int{"data_analysis": 2},
			"shared_control_plane_sha256": repeat("d", 64),
		},
		"images": []any{map[string]string{"name": "platform", "tag": "uip-package/platform:test", "package": packageName, "archive_member": "images.tar"}},
		"applications": []any{map[string]any{"code": "data_analysis", "environment": "prod", "components": []any{map[string]any{
			"service_id": "data-analysis-api", "role": "api", "conditional": false, "image": "platform",
			"registration_protocol": 2, "catalog_version": 1, "replicas": 1, "command_policy": "locked",
		}}}},
	}
	specRaw, _ := json.Marshal(specValue)
	specPath := filepath.Join(root, "spec.json")
	writeTestFile(t, specPath, specRaw)
	attestationPlan := filepath.Join(root, "attestation-plan.json")
	if err := runAttestations([]string{"--media", media, "--spec", specPath, "--profiles-dir", profilesDir, "--plan-output", attestationPlan}); err != nil {
		t.Fatalf("runAttestations: %v", err)
	}
	if err := runManifest([]string{"--media", media, "--spec", specPath, "--profiles-dir", profilesDir, "--plan-output", filepath.Join(root, "missing-plan.json")}); err == nil {
		t.Fatal("manifest must refuse before the build role signs the DSSE bundles")
	}
	buildKey := newTestKey(t)
	statementPath := filepath.Join(media, "attestations", "platform.intoto.json")
	statement, err := os.ReadFile(statementPath)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(media, "attestations", "platform.intoto.sigstore.json"), signDSSE(t, buildKey, statement))
	manifestPlan := filepath.Join(root, "release-plan.json")
	if err := runManifest([]string{"--media", media, "--spec", specPath, "--profiles-dir", profilesDir, "--plan-output", manifestPlan}); err != nil {
		t.Fatalf("runManifest: %v", err)
	}
	releaseKey := newTestKey(t)
	manifestPath := filepath.Join(media, "release-manifest.json")
	manifestRaw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(media, "release-manifest.sigstore.json"), signBlob(t, releaseKey, manifestRaw))
	trustDir := filepath.Join(root, "trust")
	writeTestFile(t, filepath.Join(trustDir, "trust-policy.json"), []byte(`{
  "schema":"urn:uip:release-trust:v1","policy":"vendor-key-offline-v1","trust_epoch":1,
  "release_keys":[{"id":"test-release","algorithm":"ecdsa-p256","public_key_pem":`+jsonString(releaseKey.public)+`}],
  "build_keys":[{"id":"test-build","algorithm":"ecdsa-p256","public_key_pem":`+jsonString(buildKey.public)+`}],
  "allowed_builders":["builder://uip/test"],
  "allowed_build_types":["https://uip.dev/buildtypes/container-v1"],
  "min_release_sequence":1
}`))
	plan, err := releaseintegrity.VerifyMedia(releaseintegrity.VerifyOptions{
		MediaDir: media, TrustDir: trustDir, Applications: []string{"data_analysis"}, RequireProvenance: true,
	})
	if err != nil {
		t.Fatalf("VerifyMedia: %v", err)
	}
	if plan.Status != "VERIFIED" || plan.ReleaseID != "test" || len(plan.Profiles) != 6 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func repeat(value string, count int) string {
	out := ""
	for len(out) < count {
		out += value
	}
	return out[:count]
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

func jsonString(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func makeTarGz(t *testing.T, path, name string, content []byte) []byte {
	t.Helper()
	// 通过精简的 tar 写入器使用生产辅助函数的格式。
	file, err := os.CreateTemp(filepath.Dir(path), ".package.")
	if err != nil {
		t.Fatal(err)
	}
	tempName := file.Name()
	defer os.Remove(tempName)
	if err := writeTarGz(file, name, content); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(tempName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return raw
}
