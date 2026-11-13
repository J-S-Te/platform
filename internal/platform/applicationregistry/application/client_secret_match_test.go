package application

import (
	"fmt"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/domain"
	"golang.org/x/crypto/bcrypt"
)

// TestClientSecretMatchRotation 验证任一当前有效密钥可匹配，错误和空输入均被拒绝。
// 参数：t为测试上下文；仅使用合成密钥及低成本测试哈希。
// 返回值：无；短路遗漏轮换旧值或错误输入被接受时失败。
func TestClientSecretMatchRotation(t *testing.T) {
	old, err := bcrypt.GenerateFromPassword([]byte("synthetic-old"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	current, err := bcrypt.GenerateFromPassword([]byte("synthetic-current"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		hashes [][]byte
		secret string
		want   bool
	}{
		{"first", [][]byte{current, old}, "synthetic-current", true},
		{"middle", [][]byte{old, current, old}, "synthetic-current", true},
		{"last", [][]byte{old, current}, "synthetic-current", true},
		{"rotation-old", [][]byte{current, old}, "synthetic-old", true},
		{"wrong", [][]byte{current, old}, "synthetic-wrong", false},
		{"empty-secret", [][]byte{current, old}, "", false},
		{"empty-set", nil, "synthetic-current", false},
		{"empty-hash", [][]byte{nil}, "synthetic-current", false},
		{"malformed-before-match", [][]byte{[]byte("invalid"), current}, "synthetic-current", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			credentials := make([]domain.ClientCredential, len(test.hashes))
			for i, hash := range test.hashes {
				credentials[i].SecretHash = hash
			}
			if got := verifyAnySecret(credentials, test.secret); got != test.want {
				t.Fatalf("credential match = %v, want %v", got, test.want)
			}
		})
	}
}

// BenchmarkCurrentClientSecretMatch 量测当前凭据首位命中时历史有效密钥造成的额外计算。
// 参数：b为基准上下文；使用真实默认成本bcrypt和合成secret，不连接任何数据库。
// 返回值：无；报告不同凭据数量的耗时，不以机器相关阈值冒充正确性断言。
func BenchmarkCurrentClientSecretMatch(b *testing.B) {
	hash, err := bcrypt.GenerateFromPassword([]byte("synthetic-current"), bcrypt.DefaultCost)
	if err != nil {
		b.Fatal(err)
	}
	for _, count := range []int{1, 8, 40} {
		b.Run(fmt.Sprintf("active-%d", count), func(b *testing.B) {
			credentials := make([]domain.ClientCredential, count)
			for i := range credentials {
				credentials[i].SecretHash = hash
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !verifyAnySecret(credentials, "synthetic-current") {
					b.Fatal("current credential rejected")
				}
			}
		})
	}
}
