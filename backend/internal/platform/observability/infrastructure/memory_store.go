package infrastructure

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/observability/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/observability/domain"
)

// MemoryStore is a process-local ring buffer for short diagnosis windows and tests. It has a fixed
// capacity and never persists runtime telemetry into MySQL. Production cross-instance queries
// should use an OTLP/observability backend adapter.
type MemoryStore struct {
	mu      sync.RWMutex
	logs    ring[domain.LogRecord]
	spans   ring[domain.TraceSpan]
	metrics ring[domain.MetricPoint]
}

// NewMemoryStore creates a bounded local telemetry buffer.
func NewMemoryStore(capacity int) *MemoryStore {
	if capacity < 1 {
		capacity = 1000
	}
	return &MemoryStore{
		logs:    newRing[domain.LogRecord](capacity),
		spans:   newRing[domain.TraceSpan](capacity),
		metrics: newRing[domain.MetricPoint](capacity),
	}
}

// AppendLog adds a defensive copy of one structured log record.
func (store *MemoryStore) AppendLog(ctx context.Context, record domain.LogRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.logs.add(copyLog(record))
	return nil
}

// AppendSpan adds a defensive copy of one trace span.
func (store *MemoryStore) AppendSpan(ctx context.Context, span domain.TraceSpan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.spans.add(copySpan(span))
	return nil
}

// AppendMetric adds a defensive copy of one metric point.
func (store *MemoryStore) AppendMetric(ctx context.Context, point domain.MetricPoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.metrics.add(copyMetric(point))
	return nil
}

// QueryLogs returns newest matching log records first.
func (store *MemoryStore) QueryLogs(ctx context.Context, query application.LogQuery) ([]domain.LogRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()

	values := store.logs.snapshot()
	result := make([]domain.LogRecord, 0, min(len(values), query.Limit))
	for index := len(values) - 1; index >= 0 && len(result) < query.Limit; index-- {
		if logMatches(values[index], query) {
			result = append(result, copyLog(values[index]))
		}
	}
	return result, nil
}

// QueryTraces returns newest matching trace spans first.
func (store *MemoryStore) QueryTraces(ctx context.Context, query application.TraceQuery) ([]domain.TraceSpan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()

	values := store.spans.snapshot()
	result := make([]domain.TraceSpan, 0, min(len(values), query.Limit))
	for index := len(values) - 1; index >= 0 && len(result) < query.Limit; index-- {
		if traceMatches(values[index], query) {
			result = append(result, copySpan(values[index]))
		}
	}
	return result, nil
}

// QueryMetrics returns newest matching metric points first.
func (store *MemoryStore) QueryMetrics(ctx context.Context, query application.MetricQuery) ([]domain.MetricPoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()

	values := store.metrics.snapshot()
	result := make([]domain.MetricPoint, 0, min(len(values), query.Limit))
	for index := len(values) - 1; index >= 0 && len(result) < query.Limit; index-- {
		if metricMatches(values[index], query) {
			result = append(result, copyMetric(values[index]))
		}
	}
	return result, nil
}

type ring[T any] struct {
	values      []T
	next, count int
}

func newRing[T any](capacity int) ring[T] {
	return ring[T]{values: make([]T, capacity)}
}

func (ring *ring[T]) add(value T) {
	ring.values[ring.next] = value
	ring.next = (ring.next + 1) % len(ring.values)
	if ring.count < len(ring.values) {
		ring.count++
	}
}

func (ring *ring[T]) snapshot() []T {
	result := make([]T, 0, ring.count)
	start := (ring.next - ring.count + len(ring.values)) % len(ring.values)
	for index := 0; index < ring.count; index++ {
		result = append(result, ring.values[(start+index)%len(ring.values)])
	}
	return result
}

func logMatches(value domain.LogRecord, query application.LogQuery) bool {
	return resourceMatches(value.Resource, query.TenantID, query.ApplicationID) &&
		stringMatches(value.TraceID, query.TraceID) &&
		stringMatches(value.RequestID, query.RequestID) &&
		stringMatches(value.Severity, query.Severity) &&
		timeMatches(value.Timestamp, query.From, query.To) &&
		keywordMatches(query.Keyword, value.Message, value.Module, value.Operation, value.ErrorCode)
}

func traceMatches(value domain.TraceSpan, query application.TraceQuery) bool {
	requestID, _ := value.Attributes["request_id"].(string)
	return resourceMatches(value.Resource, query.TenantID, query.ApplicationID) &&
		stringMatches(value.TraceID, query.TraceID) &&
		stringMatches(requestID, query.RequestID) &&
		timeMatches(value.StartedAt, query.From, query.To)
}

func metricMatches(value domain.MetricPoint, query application.MetricQuery) bool {
	return resourceMatches(value.Resource, query.TenantID, query.ApplicationID) &&
		stringMatches(value.Name, query.Name) &&
		timeMatches(value.Timestamp, query.From, query.To)
}

func resourceMatches(resource domain.ResourceLabels, tenantID, applicationID string) bool {
	return resource.TenantID == tenantID && (applicationID == "" || resource.ApplicationID == applicationID)
}

func stringMatches(value, expected string) bool {
	return expected == "" || value == expected
}

func timeMatches(value time.Time, from, to *time.Time) bool {
	return (from == nil || !value.Before(from.UTC())) && (to == nil || !value.After(to.UTC()))
}

func keywordMatches(keyword string, values ...string) bool {
	if strings.TrimSpace(keyword) == "" {
		return true
	}
	keyword = strings.ToLower(strings.TrimSpace(keyword))
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), keyword) {
			return true
		}
	}
	return false
}

func copyLog(value domain.LogRecord) domain.LogRecord {
	value.Attributes = cloneAny(value.Attributes)
	return value
}

func copySpan(value domain.TraceSpan) domain.TraceSpan {
	value.Attributes = cloneAny(value.Attributes)
	return value
}

func copyMetric(value domain.MetricPoint) domain.MetricPoint {
	value.Attributes = cloneStrings(value.Attributes)
	return value
}

func cloneAny(values map[string]any) map[string]any {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneStrings(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
