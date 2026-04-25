package usage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestRequestStatisticsRecordIncludesLatency(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
		Latency:     1500 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:  10,
			OutputTokens: 20,
			TotalTokens:  30,
		},
	})

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].LatencyMs != 1500 {
		t.Fatalf("latency_ms = %d, want 1500", details[0].LatencyMs)
	}
}

func TestRequestStatisticsRecordCapturesRequestMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stats := NewRequestStatistics()
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions?debug=1", nil)
	logging.SetGinRequestID(ginCtx, "req-12345678")
	ginCtx.Writer.WriteHeader(http.StatusAccepted)

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	stats.Record(ctx, coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
		Latency:     250 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:  5,
			OutputTokens: 7,
			TotalTokens:  12,
		},
	})

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	detail := details[0]
	if detail.RequestID != "req-12345678" {
		t.Fatalf("request_id = %q, want req-12345678", detail.RequestID)
	}
	if detail.Method != http.MethodPost {
		t.Fatalf("method = %q, want %q", detail.Method, http.MethodPost)
	}
	if detail.Path != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", detail.Path)
	}
	if detail.StatusCode != http.StatusAccepted {
		t.Fatalf("status_code = %d, want %d", detail.StatusCode, http.StatusAccepted)
	}
}

func TestRequestStatisticsMergeSnapshotDedupIgnoresLatency(t *testing.T) {
	stats := NewRequestStatistics()
	timestamp := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	first := StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"test-key": {
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						Details: []RequestDetail{{
							Timestamp: timestamp,
							LatencyMs: 0,
							Source:    "user@example.com",
							AuthIndex: "0",
							Tokens: TokenStats{
								InputTokens:  10,
								OutputTokens: 20,
								TotalTokens:  30,
							},
						}},
					},
				},
			},
		},
	}
	second := StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"test-key": {
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						Details: []RequestDetail{{
							Timestamp: timestamp,
							LatencyMs: 2500,
							Source:    "user@example.com",
							AuthIndex: "0",
							Tokens: TokenStats{
								InputTokens:  10,
								OutputTokens: 20,
								TotalTokens:  30,
							},
						}},
					},
				},
			},
		},
	}

	result := stats.MergeSnapshot(first)
	if result.Added != 1 || result.Skipped != 0 {
		t.Fatalf("first merge = %+v, want added=1 skipped=0", result)
	}

	result = stats.MergeSnapshot(second)
	if result.Added != 0 || result.Skipped != 1 {
		t.Fatalf("second merge = %+v, want added=0 skipped=1", result)
	}

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
}

func TestFilePersistenceConfigureLoadsAndFlushesSnapshot(t *testing.T) {
	stats := NewRequestStatistics()
	persistence := NewFilePersistence(stats)

	path := filepath.Join(t.TempDir(), "state", "usage-stats.json")
	payload := persistedSnapshot{
		Version: 1,
		Usage: StatisticsSnapshot{
			APIs: map[string]APISnapshot{
				"existing-key": {
					Models: map[string]ModelSnapshot{
						"gpt-5.4": {
							Details: []RequestDetail{{
								Timestamp:  time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
								Source:     "existing@example.com",
								AuthIndex:  "0",
								Method:     http.MethodPost,
								Path:       "/v1/chat/completions",
								StatusCode: http.StatusOK,
								Tokens: TokenStats{
									InputTokens:  10,
									OutputTokens: 20,
									TotalTokens:  30,
								},
							}},
						},
					},
				},
			},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	if err := persistence.Configure(path); err != nil {
		t.Fatalf("configure persistence: %v", err)
	}
	if err := persistence.Configure(path); err != nil {
		t.Fatalf("reconfigure same path: %v", err)
	}

	loaded := stats.Snapshot()
	if loaded.TotalRequests != 1 {
		t.Fatalf("loaded total_requests = %d, want 1", loaded.TotalRequests)
	}

	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "new-key",
		Model:       "gpt-5.5",
		RequestedAt: time.Date(2026, 3, 20, 13, 0, 0, 0, time.UTC),
		Detail: coreusage.Detail{
			InputTokens:  1,
			OutputTokens: 2,
			TotalTokens:  3,
		},
	})
	persistence.NotifyChanged()
	if err := persistence.Flush(); err != nil {
		t.Fatalf("flush persistence: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read flushed snapshot: %v", err)
	}
	var flushed persistedSnapshot
	if err := json.Unmarshal(raw, &flushed); err != nil {
		t.Fatalf("unmarshal flushed snapshot: %v", err)
	}
	if flushed.Usage.TotalRequests != 2 {
		t.Fatalf("flushed total_requests = %d, want 2", flushed.Usage.TotalRequests)
	}
	if _, ok := flushed.Usage.APIs["new-key"]; !ok {
		t.Fatalf("flushed snapshot missing new-key API entry")
	}
}
