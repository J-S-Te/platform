package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$`)

// 这是固定公钥离线策略的参考签名器，刻意
// 不实现无密钥签名、透明日志、TSA 或密钥托管。
// 私钥只从权限受限的本地文件读取，绝不
// 记录到日志或写入输出。
type signer struct {
	private any
}

func loadSigner(path string) (*signer, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("私钥路径必须是绝对路径")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("无法读取私钥")
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("私钥必须是安全普通文件")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("私钥权限过宽，要求 0600 或更严格")
	}
	if info.Size() <= 0 || info.Size() > 64*1024 {
		return nil, fmt.Errorf("私钥长度超出范围")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("无法读取私钥")
	}
	block, rest := pem.Decode(raw)
	if block == nil || len(rest) != 0 {
		return nil, fmt.Errorf("私钥 PEM 不唯一")
	}
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("私钥无法解析")
		}
		switch key := parsed.(type) {
		case *ecdsa.PrivateKey:
			if key.Curve == nil || key.Curve.Params().Name != "P-256" {
				return nil, fmt.Errorf("ECDSA 私钥必须是 P-256")
			}
			return &signer{private: key}, nil
		case ed25519.PrivateKey:
			return &signer{private: key}, nil
		default:
			return nil, fmt.Errorf("私钥算法不支持")
		}
	case "EC PRIVATE KEY":
		parsed, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil || parsed.Curve.Params().Name != "P-256" {
			return nil, fmt.Errorf("EC 私钥必须是 P-256")
		}
		return &signer{private: parsed}, nil
	default:
		return nil, fmt.Errorf("私钥 PEM 类型不支持（不接受加密私钥）")
	}
}

func (s *signer) signBlob(payload []byte) (map[string]any, error) {
	digest := sha256.Sum256(payload)
	switch key := s.private.(type) {
	case *ecdsa.PrivateKey:
		signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"mediaType":            "application/vnd.dev.sigstore.bundle.v0.3+json",
			"verificationMaterial": map[string]any{},
			"messageSignature": map[string]any{
				"messageDigest": map[string]string{"algorithm": "SHA2_256", "digest": base64.StdEncoding.EncodeToString(digest[:])},
				"signature":     base64.StdEncoding.EncodeToString(signature),
			},
		}, nil
	case ed25519.PrivateKey:
		signature := ed25519.Sign(key, payload)
		return map[string]any{
			"mediaType":            "application/vnd.dev.sigstore.bundle.v0.3+json",
			"verificationMaterial": map[string]any{},
			"messageSignature": map[string]any{
				"messageDigest": map[string]string{"algorithm": "SHA2_256", "digest": base64.StdEncoding.EncodeToString(digest[:])},
				"signature":     base64.StdEncoding.EncodeToString(signature),
			},
		}, nil
	default:
		return nil, fmt.Errorf("私钥算法不支持")
	}
}

func (s *signer) signDSSE(statement []byte, keyID string) (map[string]any, error) {
	const payloadType = "application/vnd.in-toto+json"
	pae := []byte(fmt.Sprintf("DSSEv1 %d %s %d %s", len(payloadType), payloadType, len(statement), statement))
	digest := sha256.Sum256(pae)
	var signature []byte
	switch key := s.private.(type) {
	case *ecdsa.PrivateKey:
		var err error
		signature, err = ecdsa.SignASN1(rand.Reader, key, digest[:])
		if err != nil {
			return nil, err
		}
	case ed25519.PrivateKey:
		signature = ed25519.Sign(key, pae)
	default:
		return nil, fmt.Errorf("私钥算法不支持")
	}
	return map[string]any{
		"mediaType":            "application/vnd.dev.sigstore.bundle.v0.3+json",
		"verificationMaterial": map[string]any{},
		"dsseEnvelope": map[string]any{
			"payloadType": payloadType,
			"payload":     base64.StdEncoding.EncodeToString(statement),
			"signatures":  []map[string]string{{"keyid": keyID, "sig": base64.StdEncoding.EncodeToString(signature)}},
		},
	}, nil
}

func main() {
	if len(os.Args) < 2 || (os.Args[1] != "blob" && os.Args[1] != "dsse") {
		fmt.Fprintln(os.Stderr, "usage: release-sign blob|dsse --key <private.pem> --input <file> --output <bundle> [--key-id <id>]")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "[错误] "+err.Error())
		os.Exit(1)
	}
}

func run(mode string, args []string) error {
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	keyPath := flags.String("key", "", "mode-0600 PKCS#8 or EC P-256 private key")
	inputPath := flags.String("input", "", "input file to sign")
	outputPath := flags.String("output", "", "new Sigstore bundle path")
	keyID := flags.String("key-id", "release-signer", "DSSE key id")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *keyPath == "" || *inputPath == "" || *outputPath == "" {
		return fmt.Errorf("--key, --input and --output are required")
	}
	if !filepath.IsAbs(*outputPath) {
		return fmt.Errorf("输出路径必须是绝对路径")
	}
	if !keyIDPattern.MatchString(*keyID) {
		return fmt.Errorf("key id 格式不正确")
	}
	inputInfo, err := os.Lstat(*inputPath)
	if err != nil || !inputInfo.Mode().IsRegular() || inputInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("输入必须是安全普通文件")
	}
	if inputInfo.Size() <= 0 || inputInfo.Size() > 32*1024*1024 {
		return fmt.Errorf("输入长度超出范围")
	}
	input, err := os.ReadFile(*inputPath)
	if err != nil {
		return fmt.Errorf("无法读取输入")
	}
	signer, err := loadSigner(*keyPath)
	if err != nil {
		return err
	}
	var payload map[string]any
	if mode == "blob" {
		payload, err = signer.signBlob(input)
	} else {
		payload, err = signer.signDSSE(input, *keyID)
	}
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if existing, err := os.Lstat(*outputPath); err == nil {
		if !existing.Mode().IsRegular() || existing.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("输出路径不是安全普通文件")
		}
		return fmt.Errorf("输出已存在，拒绝覆盖签名")
	} else if !os.IsNotExist(err) {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(*outputPath), ".release-sign.")
	if err != nil {
		return fmt.Errorf("无法创建临时输出")
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
	return os.Rename(temporaryName, *outputPath)
}
