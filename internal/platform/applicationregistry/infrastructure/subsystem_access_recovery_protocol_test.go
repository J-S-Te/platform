package infrastructure

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// TestAccessManualRecoveryUnixProtocol 验证真实Unix连接的预检、终止及读回结果。
// 参数：t为本次私有socket和受控目标文件夹的测试上下文，Docker由明确替身提供。
// 返回值：无，不将此隔离协议测试冒充真实业务系统验收。
func TestAccessManualRecoveryUnixProtocol(t *testing.T) {
	agent, _, _, request := accessRecoveryFixture(t)
	directory, err := os.MkdirTemp("/tmp", "access-recovery-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	socket := filepath.Join(directory, "agent.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 4; i++ {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			handleSubsystemProvisioningConnection(context.Background(), connection, agent)
		}
	}()
	client := &UnixSocketSubsystemProvisioner{enabled: true, socketPath: socket, timeout: time.Second}
	evidence, err := client.CheckAccessDeploymentRecovery(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.EvidenceDigest = evidence.EvidenceDigest
	value, err := client.AbandonAccessDeployment(context.Background(), request)
	if err != nil || value.State != application.AccessDeploymentAbandoned {
		t.Fatalf("wire abandon: %+v %v", value, err)
	}
	stored, found, err := client.ReadAccessDeploymentOutcome(context.Background(), request.Receipt)
	if err != nil || !found || !stored.RecordedAt.Equal(value.RecordedAt) {
		t.Fatal("wire result lost persisted decision", err)
	}
	connection, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	if err := json.NewEncoder(connection).Encode(subsystemProvisioningRequest{Version: 2,
		Action: "access-deployment-abandon", Recovery: &request, Input: &application.SubsystemProvisioningInput{}}); err != nil {
		t.Fatal(err)
	}
	var reply subsystemProvisioningReply
	if err := json.NewDecoder(connection).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if reply.Success {
		t.Fatal("mixed recovery envelope accepted")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery server did not finish")
	}
}
