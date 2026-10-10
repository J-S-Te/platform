// license-package verifies delivery licenses against the release's compiled vendor trust.
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/trust"
	core "github.com/J-S-Te/license-core"
)

type metadata struct {
	CustomerID   string   `json:"customer_id"`
	InstanceID   string   `json:"instance_id"`
	Environment  string   `json:"environment"`
	Applications []string `json:"applications"`
	Digest       string   `json:"digest"`
}

func verify(path string, keys map[string]ed25519.PublicKey, now time.Time) (metadata, error) {
	var out metadata
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > core.MaxTokenBytes {
		return out, errors.New("许可证必须是大小有效的常规文件，不能是符号链接")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out, errors.New("无法读取许可证")
	}
	l, err := core.Verify(strings.TrimSpace(string(raw)), keys)
	if err != nil {
		return out, errors.New("许可证签名或结构无效，必须使用发行版可信厂商密钥")
	}
	for _, app := range l.Applications {
		if now.Unix() >= app.ExpiresAt {
			return out, errors.New("许可证包含已到期系统，不能作为新安装交付授权")
		}
		out.Applications = append(out.Applications, app.Code)
	}
	h := sha256.Sum256(raw)
	out.CustomerID, out.InstanceID, out.Environment, out.Digest = l.CustomerID, l.InstanceID, l.Environment, hex.EncodeToString(h[:])
	return out, nil
}

func run(args []string) error {
	if len(args) == 0 || args[0] != "verify" {
		return errors.New("用法：license-package verify --file 许可证路径")
	}
	f := flag.NewFlagSet("verify", flag.ContinueOnError)
	path := f.String("file", "", "签名许可证文件")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if *path == "" || f.NArg() != 0 {
		return errors.New("必须指定 --file，且不能有额外参数")
	}
	out, err := verify(*path, trust.Keys(), time.Now())
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
