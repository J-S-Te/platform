package infrastructure

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// EmployeeImportFileGateway stores the original CSV before any IAM mutation. Credentials stay in
// the platform API process and are never returned to the browser.
type EmployeeImportFileGateway struct {
	baseURL, tokenURL, clientID, clientSecret, scope string
	httpClient                                       *http.Client
	mu                                               sync.Mutex
	token                                            string
	expiresAt                                        time.Time
}

func NewEmployeeImportFileGateway(baseURL, tokenURL, clientID, clientSecret, scope string, client *http.Client) (*EmployeeImportFileGateway, error) {
	baseURL, tokenURL = strings.TrimRight(strings.TrimSpace(baseURL), "/"), strings.TrimSpace(tokenURL)
	for _, raw := range []string{baseURL, tokenURL} {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, errors.New("IAM import gateway URLs must be HTTP(S) URLs")
		}
	}
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return nil, errors.New("IAM import gateway client credentials are required")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &EmployeeImportFileGateway{baseURL: baseURL, tokenURL: tokenURL, clientID: clientID, clientSecret: clientSecret, scope: strings.TrimSpace(scope), httpClient: client}, nil
}

func (gateway *EmployeeImportFileGateway) StoreCSV(ctx context.Context, requestID, tenantID, actorUserID string, content []byte) error {
	if len(content) == 0 || len(content) > 2<<20 || strings.TrimSpace(requestID) == "" || strings.TrimSpace(tenantID) == "" {
		return errors.New("invalid IAM import file")
	}
	digest := sha256.Sum256(content)
	payload, _ := json.Marshal(map[string]any{
		"purpose": "platform.iam.user-import", "original_name": "iam-users.csv", "media_type": "text/csv",
		"size_bytes": len(content), "sha256": hex.EncodeToString(digest[:]), "classification": "CONFIDENTIAL",
		"actor_user_id": actorUserID, "resource_type": "IAM_USER_IMPORT", "resource_id": requestID,
		"binding_type": "SOURCE", "display_name": "人员导入 CSV", "idempotency_key": requestID,
	})
	var session struct {
		Data struct {
			UploadID string `json:"upload_id"`
			FileID   string `json:"file_id"`
			Status   string `json:"status"`
		} `json:"data"`
	}
	if err := gateway.do(ctx, http.MethodPost, "/api/v2/upload-sessions", requestID, "application/json", bytes.NewReader(payload), &session); err != nil {
		return err
	}
	if session.Data.UploadID == "" || session.Data.Status != "CREATED" {
		return errors.New("IAM import upload session is not writable")
	}
	var ticket struct {
		Data struct {
			Ticket    string `json:"ticket"`
			UploadURL string `json:"upload_url"`
		} `json:"data"`
	}
	if err := gateway.do(ctx, http.MethodPost, "/api/v2/upload-sessions/"+url.PathEscape(session.Data.UploadID)+"/tickets", requestID, "application/json", nil, &ticket); err != nil {
		return err
	}
	uploadPath := strings.TrimPrefix(ticket.Data.UploadURL, "/file-gateway")
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, gateway.baseURL+uploadPath, bytes.NewReader(content))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "UploadTicket "+ticket.Data.Ticket)
	req.Header.Set("Content-Type", "text/csv")
	response, err := gateway.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload IAM import: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return fmt.Errorf("file gateway returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (gateway *EmployeeImportFileGateway) do(ctx context.Context, method, path, requestID, contentType string, body io.Reader, target any) error {
	token, err := gateway.bearer(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, gateway.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Request-ID", requestID)
	req.Header.Set("Idempotency-Key", requestID)
	response, err := gateway.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call file gateway: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return fmt.Errorf("file gateway returned HTTP %d", response.StatusCode)
	}
	if target != nil {
		return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(target)
	}
	return nil
}

func (gateway *EmployeeImportFileGateway) bearer(ctx context.Context) (string, error) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if gateway.token != "" && time.Now().Add(30*time.Second).Before(gateway.expiresAt) {
		return gateway.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {gateway.scope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(gateway.clientID, gateway.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := gateway.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("obtain file gateway token: %w", err)
	}
	defer response.Body.Close()
	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result) != nil || result.AccessToken == "" {
		return "", errors.New("obtain file gateway token")
	}
	if result.ExpiresIn <= 0 {
		result.ExpiresIn = 300
	}
	gateway.token, gateway.expiresAt = result.AccessToken, time.Now().Add(time.Duration(result.ExpiresIn)*time.Second)
	return gateway.token, nil
}
