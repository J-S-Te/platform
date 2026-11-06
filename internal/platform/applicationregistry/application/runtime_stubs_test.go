package application

import (
	"context"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
)

// runtimeClientsStub 与 runtimeRegistrarStub 是 V2 内置目录登记测试共用的最小桩；
// V1 审批测试下线后集中放在这里，避免与业务断言混写。
type runtimeClientsStub struct {
	clients   []OAuthClientView
	created   []OAuthClientCreateInput
	rotations int
}

func (s *runtimeClientsStub) ListOAuthClients(context.Context, string) ([]OAuthClientView, error) {
	return s.clients, nil
}

func (s *runtimeClientsStub) CreateOAuthClient(_ context.Context, in OAuthClientCreateInput) (OAuthClientCreateResult, error) {
	s.created = append(s.created, in)
	c := OAuthClientView{ID: in.ClientID, ClientID: in.ClientID, ApplicationID: in.ApplicationID, EnvironmentID: in.EnvironmentID, ClientType: in.ClientType, TokenAuthMethod: in.TokenAuthMethod, GrantTypes: in.GrantTypes, Scopes: in.Scopes, Status: "ACTIVE"}
	s.clients = append(s.clients, c)
	return OAuthClientCreateResult{Client: c, PlaintextSecret: "isolated-" + in.ClientID}, nil
}

func (s *runtimeClientsStub) CreateOAuthClientSecret(context.Context, OAuthClientSecretCreateInput) (OAuthClientSecretResult, error) {
	s.rotations++
	return OAuthClientSecretResult{PlaintextSecret: "replacement"}, nil
}

type runtimeRegistrarStub struct {
	specs []coordination.ServiceSpec
	err   error
}

func (s *runtimeRegistrarStub) Register(_ context.Context, spec coordination.ServiceSpec) error {
	s.specs = append(s.specs, spec)
	return s.err
}

// statusRegistrarStub 提供固定 Status 的登记器，供确认流程测试使用。
type statusRegistrarStub struct {
	status coordination.Status
	specs  []coordination.ServiceSpec
	err    error
}

func (s *statusRegistrarStub) Status(context.Context, string) (coordination.Status, error) {
	return s.status, nil
}

func (s *statusRegistrarStub) Register(_ context.Context, spec coordination.ServiceSpec) error {
	s.specs = append(s.specs, spec)
	return s.err
}
