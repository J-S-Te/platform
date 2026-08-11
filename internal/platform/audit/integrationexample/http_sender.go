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

// TokenSource resolves the short-lived application Bearer token at send time. Its returned value
// must only be placed in the Authorization header and must never be persisted in an outbox row.
type TokenSource interface {
	Token(context.Context) (string, error)
}

// HTTPSender calls Basic Platform's application-authenticated batch ingestion endpoint.
type HTTPSender struct {
	baseURL     *url.URL
	httpClient  *http.Client
	tokenSource TokenSource
}

// NewHTTPSender validates the configured platform base URL and dependencies.
func NewHTTPSender(baseURL string, httpClient *http.Client, tokenSource TokenSource) (*HTTPSender, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || httpClient == nil || tokenSource == nil {
		return nil, errors.New("audit HTTP sender configuration is invalid")
	}
	return &HTTPSender{baseURL: parsed, httpClient: httpClient, tokenSource: tokenSource}, nil
}

// Send posts no more than 100 events and returns the platform's batch receipts. Only stable error
// descriptions are returned so callers do not accidentally log response bodies containing details.
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
