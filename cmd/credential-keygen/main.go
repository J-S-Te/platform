// credential-keygen creates one RSA-2048 keypair and keyring metadata for
// uip-credential/v1. It never prints or overwrites private keys.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

type keyMetadata struct {
	KID            string `json:"kid"`
	PrivateKeyFile string `json:"private_key_file"`
	PublicKeyFile  string `json:"public_key_file"`
}

type keyringMetadata struct {
	ActiveKID string        `json:"active_kid"`
	Keys      []keyMetadata `json:"keys"`
}

func main() {
	outputDir := flag.String("dir", "", "directory for the keyring and PEM files")
	name := flag.String("name", "credential-keyring", "base name for keyring metadata and key files")
	flag.Parse()
	if *outputDir == "" {
		fmt.Fprintln(os.Stderr, "--dir is required")
		os.Exit(2)
	}
	if err := os.MkdirAll(*outputDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "create output directory: %v\n", err)
		os.Exit(1)
	}
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate RSA key: %v\n", err)
		os.Exit(1)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal private key: %v\n", err)
		os.Exit(1)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal public key: %v\n", err)
		os.Exit(1)
	}
	kid, err := kidForPublicKey(publicDER)
	if err != nil {
		fmt.Fprintf(os.Stderr, "derive kid: %v\n", err)
		os.Exit(1)
	}
	privateName := *name + "-" + kid + ".private.pem"
	publicName := *name + "-" + kid + ".public.pem"
	if err := writeExclusive(filepath.Join(*outputDir, privateName), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write private key: %v\n", err)
		os.Exit(1)
	}
	if err := writeExclusive(filepath.Join(*outputDir, publicName), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write public key: %v\n", err)
		os.Exit(1)
	}
	metadata, err := json.MarshalIndent(keyringMetadata{ActiveKID: kid, Keys: []keyMetadata{{KID: kid, PrivateKeyFile: privateName, PublicKeyFile: publicName}}}, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode keyring: %v\n", err)
		os.Exit(1)
	}
	metadataPath := filepath.Join(*outputDir, *name+".json")
	if err := writeExclusive(metadataPath, append(metadata, '\n'), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write keyring: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("kid=%s\nkeyring=%s\n", kid, metadataPath)
}

func kidForPublicKey(der []byte) (string, error) {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

func writeExclusive(path string, value []byte, mode os.FileMode) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("refusing to overwrite existing file %s", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(value)
	return err
}
