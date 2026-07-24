// generate-ed25519-jwt-key-pair creates the PEM-encoded key files required by
// Basic Platform's JWT implementation. It deliberately uses only the Go
// standard library so it remains usable on older Linux distributions whose
// OpenSSL packages do not provide the ED25519 algorithm.
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

	// O_EXCL ensures the caller never overwrites an existing key accidentally.
	// The bootstrap script passes fresh staging paths and installs the completed
	// pair into its final paths only after both files have been generated.
	if err := writeExclusive(privatePath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
		fatalf("write private key: %v", err)
	}
	if err := writeExclusive(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0o644); err != nil {
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
