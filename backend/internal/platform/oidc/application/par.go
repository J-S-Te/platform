package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/domain"
)

const maxPARTTL = 90 * time.Second

// PushedAuthorizationRequest 是 PAR 的短期一次性状态，只保存规范化参数与敏感值摘要；
// 原始 request object 和可兑换 request_uri 都不落库，数据库泄露时不能直接重放浏览器授权。
type PushedAuthorizationRequest struct {
	ID, TenantID, OAuthClientID                      string
	RequestURIHash                                   [32]byte
	RedirectURI                                      string
	Scopes                                           []string
	State, Nonce, CodeChallenge, CodeChallengeMethod string
	RequestObjectHash                                *[32]byte
	CreatedAt, ExpiresAt                             time.Time
}

// PushAuthorizationRequestInput is a client-authenticated PAR request. RequestObjectAudience is
// supplied by the trusted HTTP adapter and must be the provider's authorization endpoint URI.
type PushAuthorizationRequestInput struct {
	ClientAuthentication
	ResponseType                                     string
	RedirectURI                                      string
	Scopes                                           []string
	State, Nonce, CodeChallenge, CodeChallengeMethod string
	RequestObject, RequestObjectAudience             string
	TTL                                              time.Duration
}

type PushAuthorizationRequestResult struct {
	RequestURI string
	ExpiresIn  int64
}

type ConsumePushedAuthorizationRequestInput struct{ ClientID, RequestURI, SessionID string }

// RequestObjectAuthorizationInput represents a signed request object submitted directly to the
// authorization endpoint. SessionID is deliberately supplied only after browser authentication.
type RequestObjectAuthorizationInput struct {
	AuthorizationInput
	ResponseType          string
	RequestObject         string
	RequestObjectAudience string
}

type PushedAuthorizationRequestRepository interface {
	CreatePushedAuthorizationRequest(context.Context, PushedAuthorizationRequest) error
	ConsumePushedAuthorizationRequest(context.Context, string, string, [32]byte, time.Time) (PushedAuthorizationRequest, error)
}

// PushAuthorizationRequest 在后端通道认证客户端，并把普通参数与可选签名请求对象合并成唯一规范形态。
// 浏览器随后只携带短期 request_uri，避免长授权参数经过地址栏、历史记录和代理日志。
func (service *Service) PushAuthorizationRequest(ctx context.Context, input PushAuthorizationRequestInput) (PushAuthorizationRequestResult, error) {
	input = normalizePARInput(input)
	if input.ClientID == "" || input.ResponseType != "code" || input.TTL <= 0 || input.TTL > maxPARTTL {
		return PushAuthorizationRequestResult{}, ErrInvalidRequest
	}
	now := service.now()
	client, err := service.authenticatedClient(ctx, input.ClientAuthentication, now)
	if err != nil {
		return PushAuthorizationRequestResult{}, err
	}
	if !has(client.GrantTypes, "authorization_code") {
		return PushAuthorizationRequestResult{}, ErrUnauthorizedClient
	}
	input, requestHash, err := resolvePARRequestObject(input, client, now)
	if err != nil {
		return PushAuthorizationRequestResult{}, ErrInvalidRequest
	}
	if input.RedirectURI == "" {
		return PushAuthorizationRequestResult{}, ErrInvalidRequest
	}
	if _, ok := client.RedirectURIs[input.RedirectURI]; !ok {
		return PushAuthorizationRequestResult{}, ErrInvalidRequest
	}
	if _, err = registeredScopes(client, input.Scopes); err != nil {
		return PushAuthorizationRequestResult{}, err
	}
	if _, _, err = validatePKCE(client.RequirePKCE, input.CodeChallenge, input.CodeChallengeMethod); err != nil {
		return PushAuthorizationRequestResult{}, err
	}
	if !validProtocolText(input.State, 2048) || !validProtocolText(input.Nonce, 255) {
		return PushAuthorizationRequestResult{}, ErrInvalidRequest
	}

	id, err := service.ids.New(now)
	if err != nil {
		return PushAuthorizationRequestResult{}, fmt.Errorf("generate PAR id: %w", err)
	}
	secret, err := service.secrets.NewSecret()
	if err != nil || !validOpaqueSecret(secret) {
		return PushAuthorizationRequestResult{}, errors.New("generate PAR request URI secret")
	}
	requestURI := "urn:ietf:params:oauth:request_uri:" + secret
	repository, ok := service.repository.(PushedAuthorizationRequestRepository)
	if !ok {
		return PushAuthorizationRequestResult{}, errors.New("PAR persistence is not configured")
	}
	expires := now.Add(input.TTL)
	request := PushedAuthorizationRequest{
		ID: id, TenantID: client.TenantID, OAuthClientID: client.ID, RequestURIHash: digest(requestURI),
		RedirectURI: input.RedirectURI, Scopes: normalizeScopes(input.Scopes), State: input.State, Nonce: input.Nonce,
		CodeChallenge: input.CodeChallenge, CodeChallengeMethod: input.CodeChallengeMethod, RequestObjectHash: requestHash,
		CreatedAt: now, ExpiresAt: expires,
	}
	// request_uri 与授权码一样按摘要检索；返回给客户端的 bearer 值只在后续 /authorize 使用一次。
	if err = repository.CreatePushedAuthorizationRequest(ctx, request); err != nil {
		return PushAuthorizationRequestResult{}, fmt.Errorf("create PAR: %w", err)
	}
	return PushAuthorizationRequestResult{RequestURI: requestURI, ExpiresIn: int64(input.TTL / time.Second)}, nil
}

// ConsumePushedAuthorizationRequest resolves and atomically consumes a PAR URI for the same
// client. A request URI is never reusable and cannot be transferred to another client.
func (service *Service) ConsumePushedAuthorizationRequest(ctx context.Context, input ConsumePushedAuthorizationRequestInput) (AuthorizationInput, error) {
	input.ClientID = strings.TrimSpace(input.ClientID)
	input.RequestURI = strings.TrimSpace(input.RequestURI)
	input.SessionID = strings.TrimSpace(input.SessionID)
	if input.ClientID == "" || !validPARRequestURI(input.RequestURI) || input.SessionID == "" {
		return AuthorizationInput{}, ErrInvalidRequest
	}
	client, err := service.repository.FindClient(ctx, input.ClientID, service.now())
	if err != nil {
		return AuthorizationInput{}, mapClientError(err)
	}
	repository, ok := service.repository.(PushedAuthorizationRequestRepository)
	if !ok {
		return AuthorizationInput{}, ErrInvalidRequest
	}
	request, err := repository.ConsumePushedAuthorizationRequest(ctx, client.TenantID, client.ID, digest(input.RequestURI), service.now())
	if err != nil {
		return AuthorizationInput{}, mapGrantError(err)
	}
	return AuthorizationInput{
		ClientID: client.ClientID, RedirectURI: request.RedirectURI, Scopes: request.Scopes, State: request.State,
		Nonce: request.Nonce, CodeChallenge: request.CodeChallenge, CodeChallengeMethod: request.CodeChallengeMethod,
		SessionID: input.SessionID,
	}, nil
}

// ResolveRequestObject 验证直接提交给 /authorize 的 JAR，并与查询参数逐项合并。
// 同一字段若在两处出现必须语义一致，不能用“签名值优先”掩盖外层参数被篡改或客户端实现错误。
func (service *Service) ResolveRequestObject(ctx context.Context, input RequestObjectAuthorizationInput) (AuthorizationInput, error) {
	input.AuthorizationInput = normalizeAuthorizationInput(input.AuthorizationInput)
	input.ResponseType = strings.TrimSpace(input.ResponseType)
	input.RequestObject = strings.TrimSpace(input.RequestObject)
	input.RequestObjectAudience = strings.TrimSpace(input.RequestObjectAudience)
	if input.ClientID == "" || input.ResponseType != "code" || input.RequestObject == "" || input.RequestObjectAudience == "" {
		return AuthorizationInput{}, ErrInvalidRequest
	}
	client, err := service.repository.FindClient(ctx, input.ClientID, service.now())
	if err != nil {
		return AuthorizationInput{}, mapClientError(err)
	}
	if !has(client.GrantTypes, "authorization_code") {
		return AuthorizationInput{}, ErrUnauthorizedClient
	}
	fields, err := validateRequestObject(input.RequestObject, client, input.RequestObjectAudience, service.now())
	if err != nil {
		return AuthorizationInput{}, ErrInvalidRequest
	}
	if fields.ResponseType != "code" {
		return AuthorizationInput{}, ErrInvalidRequest
	}
	resolved, err := mergeAuthorizationInput(input.AuthorizationInput, fields)
	if err != nil {
		return AuthorizationInput{}, ErrInvalidRequest
	}
	return resolved, nil
}

type requestObjectFields struct {
	ClientID, ResponseType, RedirectURI, Scope, State, Nonce, CodeChallenge, CodeChallengeMethod string
}

func validateRequestObject(raw string, client domain.Client, audience string, now time.Time) (requestObjectFields, error) {
	// JAR 的受众固定为平台授权端点，而非 token 端点；短 exp 和可选 iat/nbf 时间窗限制了
	// 被截获请求对象的可重放周期。真正的一次性语义则由 PAR request_uri 的消费事务提供。
	token, err := parseCompactJWT(raw)
	if err != nil || verifyCompactJWT(token, client.JWKs) != nil {
		return requestObjectFields{}, errors.New("invalid request object")
	}
	issuer, ok := jwtString(token.Claims, "iss")
	if !ok || issuer != client.ClientID {
		return requestObjectFields{}, errors.New("invalid request object issuer")
	}
	if !jwtAudienceContains(token.Claims["aud"], audience) {
		return requestObjectFields{}, errors.New("invalid request object audience")
	}
	exp, ok := jwtTime(token.Claims, "exp")
	if !ok || !exp.After(now) || exp.After(now.Add(5*time.Minute)) {
		return requestObjectFields{}, errors.New("invalid request object expiry")
	}
	if issuedAt, present, timeErr := jwtOptionalTime(token.Claims, "iat"); timeErr != nil || (present && (issuedAt.After(now.Add(time.Minute)) || issuedAt.Before(now.Add(-5*time.Minute)))) {
		return requestObjectFields{}, errors.New("invalid request object issued at")
	}
	if notBefore, present, timeErr := jwtOptionalTime(token.Claims, "nbf"); timeErr != nil || (present && notBefore.After(now.Add(time.Minute))) {
		return requestObjectFields{}, errors.New("invalid request object not before")
	}
	if jti, ok := jwtString(token.Claims, "jti"); ok && !validProtocolText(jti, 512) {
		return requestObjectFields{}, errors.New("invalid request object jti")
	}
	clientID, ok := jwtString(token.Claims, "client_id")
	if !ok || clientID != client.ClientID {
		return requestObjectFields{}, errors.New("invalid request object client")
	}
	responseType, ok := jwtString(token.Claims, "response_type")
	if !ok || responseType != "code" {
		return requestObjectFields{}, errors.New("invalid request object response type")
	}
	redirect, _ := jwtString(token.Claims, "redirect_uri")
	scope, _ := jwtString(token.Claims, "scope")
	state, _ := jwtString(token.Claims, "state")
	nonce, _ := jwtString(token.Claims, "nonce")
	challenge, _ := jwtString(token.Claims, "code_challenge")
	method, _ := jwtString(token.Claims, "code_challenge_method")
	return requestObjectFields{
		ClientID: clientID, ResponseType: responseType, RedirectURI: redirect, Scope: scope, State: state,
		Nonce: nonce, CodeChallenge: challenge, CodeChallengeMethod: method,
	}, nil
}

func resolvePARRequestObject(input PushAuthorizationRequestInput, client domain.Client, now time.Time) (PushAuthorizationRequestInput, *[32]byte, error) {
	if input.RequestObject == "" {
		return input, nil, nil
	}
	if input.RequestObjectAudience == "" {
		return PushAuthorizationRequestInput{}, nil, errors.New("request object audience is required")
	}
	fields, err := validateRequestObject(input.RequestObject, client, input.RequestObjectAudience, now)
	if err != nil || fields.ResponseType != input.ResponseType {
		return PushAuthorizationRequestInput{}, nil, errors.New("invalid request object")
	}
	merged, err := mergePushAuthorizationRequestInput(input, fields)
	if err != nil {
		return PushAuthorizationRequestInput{}, nil, err
	}
	hash := digest(input.RequestObject)
	return merged, &hash, nil
}

func mergePushAuthorizationRequestInput(input PushAuthorizationRequestInput, value requestObjectFields) (PushAuthorizationRequestInput, error) {
	if input.ClientID != value.ClientID {
		return PushAuthorizationRequestInput{}, errors.New("request object client conflict")
	}
	redirect, scope, state, nonce, challenge, method, err := mergeRequestFields(
		input.RedirectURI, strings.Join(input.Scopes, " "), input.State, input.Nonce, input.CodeChallenge, input.CodeChallengeMethod,
		value.RedirectURI, value.Scope, value.State, value.Nonce, value.CodeChallenge, value.CodeChallengeMethod,
	)
	if err != nil {
		return PushAuthorizationRequestInput{}, err
	}
	input.RedirectURI, input.Scopes, input.State, input.Nonce, input.CodeChallenge, input.CodeChallengeMethod = redirect, normalizeScopes(strings.Fields(scope)), state, nonce, challenge, method
	return input, nil
}

func mergeAuthorizationInput(input AuthorizationInput, value requestObjectFields) (AuthorizationInput, error) {
	if input.ClientID != value.ClientID {
		return AuthorizationInput{}, errors.New("request object client conflict")
	}
	redirect, scope, state, nonce, challenge, method, err := mergeRequestFields(
		input.RedirectURI, strings.Join(input.Scopes, " "), input.State, input.Nonce, input.CodeChallenge, input.CodeChallengeMethod,
		value.RedirectURI, value.Scope, value.State, value.Nonce, value.CodeChallenge, value.CodeChallengeMethod,
	)
	if err != nil {
		return AuthorizationInput{}, err
	}
	input.RedirectURI, input.Scopes, input.State, input.Nonce, input.CodeChallenge, input.CodeChallengeMethod = redirect, normalizeScopes(strings.Fields(scope)), state, nonce, challenge, method
	return input, nil
}

func mergeRequestFields(redirect, scope, state, nonce, challenge, method, signedRedirect, signedScope, signedState, signedNonce, signedChallenge, signedMethod string) (string, string, string, string, string, string, error) {
	merge := func(direct, signed string) (string, error) {
		if direct != "" && signed != "" && direct != signed {
			return "", errors.New("request object conflict")
		}
		if direct != "" {
			return direct, nil
		}
		return signed, nil
	}
	mergedRedirect, err := merge(redirect, signedRedirect)
	if err != nil {
		return "", "", "", "", "", "", err
	}
	// scope 是无序集合，比较前先排序去重；否则仅顺序不同也会被误判成签名内容冲突。
	canonicalScope := strings.Join(normalizeScopes(strings.Fields(scope)), " ")
	canonicalSignedScope := strings.Join(normalizeScopes(strings.Fields(signedScope)), " ")
	mergedScope, err := merge(canonicalScope, canonicalSignedScope)
	if err != nil {
		return "", "", "", "", "", "", err
	}
	mergedState, err := merge(state, signedState)
	if err != nil {
		return "", "", "", "", "", "", err
	}
	mergedNonce, err := merge(nonce, signedNonce)
	if err != nil {
		return "", "", "", "", "", "", err
	}
	mergedChallenge, err := merge(challenge, signedChallenge)
	if err != nil {
		return "", "", "", "", "", "", err
	}
	mergedMethod, err := merge(method, signedMethod)
	if err != nil {
		return "", "", "", "", "", "", err
	}
	return mergedRedirect, mergedScope, mergedState, mergedNonce, mergedChallenge, mergedMethod, nil
}

func validPARRequestURI(value string) bool {
	const prefix = "urn:ietf:params:oauth:request_uri:"
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	secret := strings.TrimPrefix(value, prefix)
	if len(secret) < 43 || len(secret) > 512 {
		return false
	}
	for _, character := range secret {
		if !(character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' || character == '_') {
			return false
		}
	}
	return true
}

func normalizePARInput(input PushAuthorizationRequestInput) PushAuthorizationRequestInput {
	input.ClientID = strings.TrimSpace(input.ClientID)
	input.ResponseType = strings.TrimSpace(input.ResponseType)
	input.RedirectURI = strings.TrimSpace(input.RedirectURI)
	input.Scopes = normalizeScopes(input.Scopes)
	input.State = strings.TrimSpace(input.State)
	input.Nonce = strings.TrimSpace(input.Nonce)
	input.CodeChallenge = strings.TrimSpace(input.CodeChallenge)
	input.CodeChallengeMethod = strings.TrimSpace(input.CodeChallengeMethod)
	input.RequestObject = strings.TrimSpace(input.RequestObject)
	input.RequestObjectAudience = strings.TrimSpace(input.RequestObjectAudience)
	return input
}
