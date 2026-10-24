package infrastructure

import (
	"context"
	"errors"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type imageReaderStub struct {
	images   []application.SubsystemImageReadiness
	reported bool
	err      error
}

func (stub imageReaderStub) ReadSubsystemAccessImages(context.Context, string, string) ([]application.SubsystemImageReadiness, bool, error) {
	return stub.images, stub.reported, stub.err
}

// 三态：未上报 → 未验证；全部存在 → 就绪；有缺失 → 不就绪（含缺失服务清单）。
func TestSubsystemAccessImageReadinessAdapterThreeStates(t *testing.T) {
	adapter, err := NewSubsystemAccessImageReadinessAdapter(imageReaderStub{})
	if err != nil {
		t.Fatal(err)
	}
	ready, version, verified, err := adapter.ReadSubsystemAccessImageReadiness(context.Background(), "tenant-1", "settlement", "dev")
	if err != nil || verified || ready || version != "unreported" {
		t.Fatalf("unreported: ready=%v version=%s verified=%v err=%v", ready, version, verified, err)
	}

	present := []application.SubsystemImageReadiness{
		{ServiceID: "settlement-api", ImageDigest: "sha256:aaa", Present: true},
		{ServiceID: "settlement-worker", ImageDigest: "sha256:bbb", Present: true},
	}
	adapter, _ = NewSubsystemAccessImageReadinessAdapter(imageReaderStub{images: present, reported: true})
	ready, version, verified, err = adapter.ReadSubsystemAccessImageReadiness(context.Background(), "tenant-1", "settlement", "dev")
	if err != nil || !verified || !ready || version != "images:2" {
		t.Fatalf("all present: ready=%v version=%s verified=%v err=%v", ready, version, verified, err)
	}

	missing := append([]application.SubsystemImageReadiness{{ServiceID: "settlement-migrate", ImageDigest: "sha256:ccc", Present: false}}, present...)
	adapter, _ = NewSubsystemAccessImageReadinessAdapter(imageReaderStub{images: missing, reported: true})
	ready, version, verified, err = adapter.ReadSubsystemAccessImageReadiness(context.Background(), "tenant-1", "settlement", "dev")
	if err != nil || !verified || ready || version != "missing:settlement-migrate" {
		t.Fatalf("missing image: ready=%v version=%s verified=%v err=%v", ready, version, verified, err)
	}

	// 读取失败：向上返回错误，由就绪适配器决定（当前语义是整体失败，不猜测）。
	failing, _ := NewSubsystemAccessImageReadinessAdapter(imageReaderStub{err: errors.New("agent unavailable")})
	if _, _, _, err := failing.ReadSubsystemAccessImageReadiness(context.Background(), "tenant-1", "settlement", "dev"); err == nil {
		t.Fatal("reader failure must surface as an error")
	}
	if _, err := NewSubsystemAccessImageReadinessAdapter(nil); err == nil {
		t.Fatal("nil reader must fail construction")
	}
}
