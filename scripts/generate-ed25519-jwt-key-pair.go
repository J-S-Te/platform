// generate-ed25519-jwt-key-pair 生成平台 JWT 所需的 PEM 密钥对。只依赖 Go 标准库，
// 因而可在系统 OpenSSL 尚不支持 Ed25519 的旧 Linux 发行版上用于首次部署。
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		fatalf("usage: %s <private-key-path> <public-key-path>", os.Args[0])
	}

	privatePath := os.Args[1]
	publicPath := os.Args[2]
	if privatePath == publicPath {
		fatalf("private and public key paths must differ")
	}

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fatalf("generate Ed25519 key pair: %v", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		fatalf("encode Ed25519 private key as PKCS#8: %v", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		fatalf("encode Ed25519 public key as PKIX: %v", err)
	}

	// O_EXCL 防止误覆盖现有密钥。部署脚本传入新的暂存路径，只有公私钥都成功落盘后
	// 才安装到最终位置，避免只轮换其中一把导致所有实例无法互相验签。
	if err := writeExclusive(privatePath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
		fatalf("write private key: %v", err)
	}
	if err := writeExclusive(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0o644); err != nil {
		// 公钥写入失败时删除本次新建的私钥，使调用方可以安全重试，不留下半套密钥。
		_ = os.Remove(privatePath)
		fatalf("write public key: %v", err)
	}
}

func writeExclusive(path string, contents []byte, mode os.FileMode) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := file.Close()
		if err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	if _, err = file.Write(contents); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	return nil
}

func fatalf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
