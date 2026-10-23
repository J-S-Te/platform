package integrationexample

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// TokenSource 在发送时刻解析短时效的应用 Bearer token。返回值只能放进 Authorization 头，
// 绝不能持久化到 outbox 行。
type TokenSource interface {
	Token(context.Context) (string, error)
}

// HTTPSender 调用基础平台的应用认证批量摄取端点。
type HTTPSender struct {
	baseURL     *url.URL
	httpClient  *http.Client
	tokenSource TokenSource
}

// NewHTTPSender 校验配置的平台基地址与依赖。
func NewHTTPSender(baseURL string, httpClient *http.Client, tokenSource TokenSource) (*HTTPSender, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || httpClient == nil || tokenSource == nil {
		return nil, errors.New("audit HTTP sender configuration is invalid")
	}
	return &HTTPSender{baseURL: parsed, httpClient: httpClient, tokenSource: tokenSource}, nil
}

// Send 提交至多 100 条事件并返回平台批量回执。只返回稳定的错误描述，
// 防止调用方把包含细节的响应体误写进日志。
func (sender *HTTPSender) Send(ctx context.Context, events []Event) ([]Receipt, error) {
	if len(events) == 0 || len(events) > maxBatchSize {
		return nil, errors.New("audit event batch must contain 1 to 100 items")
	}
	token, err := sender.tokenSource.Token(ctx)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, errors.New("resolve audit application token")
	}
	body, err := json.Marshal(struct {
		Events []Event `json:"events"`
	}{Events: events})
	if err != nil {
		return nil, fmt.Errorf("encode audit event batch: %w", err)
	}
	endpoint := sender.baseURL.ResolveReference(&url.URL{Path: AuditIngestPath})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create audit ingestion request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	response, err := sender.httpClient.Do(request)
	if err != nil {
		return nil, errors.New("audit ingestion request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return nil, fmt.Errorf("audit ingestion returned status %d", response.StatusCode)
	}
	var envelope struct {
		Code string    `json:"code"`
		Data []Receipt `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope); err != nil {
		return nil, errors.New("decode audit ingestion receipt")
	}
	if envelope.Code != "OK" {
		return nil, errors.New("audit ingestion returned an invalid response envelope")
	}
	return envelope.Data, nil
}
