package cmd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/cmd"
	"github.com/foundryfabric/clusterbox/internal/registry"
)

// seedHistory inserts a fixed set of history rows into reg, spanning two
// clusters and two services with deterministic timestamps. Returns the
// inserted entries in chronological (oldest-first) order so tests can
// reason about reverse-chronological output.
func seedHistory(t *testing.T, reg registry.Registry) []registry.DeploymentHistoryEntry {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)

	entries := []registry.DeploymentHistoryEntry{
		{
			ClusterName: "alpha", Service: "api", Version: "v1.0.0",
			AttemptedAt: base, Status: registry.StatusRolledOut, RolloutDurationMs: 250,
		},
		{
			ClusterName: "alpha", Service: "worker", Version: "v1.0.1",
			AttemptedAt: base.Add(1 * time.Minute), Status: registry.StatusFailed,
			RolloutDurationMs: 4500, Error: "node alpha-w-1 unreachable",
		},
		{
			ClusterName: "beta", Service: "api", Version: "v2.0.0",
			AttemptedAt: base.Add(2 * time.Minute), Status: registry.StatusRolledOut,
			RolloutDurationMs: 1500,
		},
		{
			ClusterName: "beta", Service: "api", Version: "v2.0.1",
			AttemptedAt: base.Add(3 * time.Minute), Status: registry.StatusRolledOut,
			RolloutDurationMs: 999,
		},
	}
	for i := range entries {
		if err := reg.AppendHistory(ctx, entries[i]); err != nil {
			t.Fatalf("seed AppendHistory: %v", err)
		}
	}
	return entries
}

// TestHistoryEmpty verifies the empty-result hint and exit-success.
func TestHistoryEmpty(t *testing.T) {
	reg := newTempRegistry(t)

	var buf bytes.Buffer
	if err := cmd.RunHistory(context.Background(), reg, &buf, registry.HistoryFilter{Limit: 50}, false); err != nil {
		t.Fatalf("RunHistory: %v", err)
	}
	if got, want := buf.String(), "no history matches.\n"; got != want {
		t.Errorf("empty output mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestHistoryNoFilter verifies that every row is returned in
// reverse-chronological order with the expected header.
func TestHistoryNoFilter(t *testing.T) {
	reg := newTempRegistry(t)
	seedHistory(t, reg)

	var buf bytes.Buffer
	if err := cmd.RunHistory(context.Background(), reg, &buf, registry.HistoryFilter{Limit: 50}, false); err != nil {
		t.Fatalf("RunHistory: %v", err)
	}
	got := buf.String()

	if !strings.Contains(got, "WHEN") || !strings.Contains(got, "ERROR") {
		t.Errorf("expected header row with WHEN and ERROR, got:\n%s", got)
	}
	// Reverse-chronological: v2.0.1 (newest) must precede v1.0.0 (oldest).
	newIdx := strings.Index(got, "v2.0.1")
	oldIdx := strings.Index(got, "v1.0.0")
	if newIdx < 0 || oldIdx < 0 {
		t.Fatalf("expected both versions in output:\n%s", got)
	}
	if newIdx > oldIdx {
		t.Errorf("expected v2.0.1 (newest) before v1.0.0 (oldest), got:\n%s", got)
	}

	// WHEN format spot-check (UTC, no seconds, no zone suffix).
	if !strings.Contains(got, "2026-04-20 12:00") {
		t.Errorf("expected formatted WHEN timestamp, got:\n%s", got)
	}
	// Duration formatting: 250ms < 1000 -> "ms"; 1500ms >= 1000 -> "s".
	if !strings.Contains(got, "250ms") {
		t.Errorf("expected 250ms in output:\n%s", got)
	}
	if !strings.Contains(got, "1s") {
		t.Errorf("expected duration 1s (from 1500ms) in output:\n%s", got)
	}
	// 999ms remains in milliseconds (boundary check).
	if !strings.Contains(got, "999ms") {
		t.Errorf("expected 999ms in output:\n%s", got)
	}
}

// TestHistoryClusterFilter verifies --cluster narrows results to one cluster.
func TestHistoryClusterFilter(t *testing.T) {
	reg := newTempRegistry(t)
	seedHistory(t, reg)

	var buf bytes.Buffer
	if err := cmd.RunHistory(context.Background(), reg, &buf,
		registry.HistoryFilter{ClusterName: "alpha", Limit: 50}, false); err != nil {
		t.Fatalf("RunHistory: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "beta") {
		t.Errorf("expected only alpha rows, found beta:\n%s", got)
	}
	if !strings.Contains(got, "alpha") {
		t.Errorf("expected alpha rows, got:\n%s", got)
	}
}

// TestHistoryServiceFilter verifies --service narrows results to one service.
func TestHistoryServiceFilter(t *testing.T) {
	reg := newTempRegistry(t)
	seedHistory(t, reg)

	var buf bytes.Buffer
	if err := cmd.RunHistory(context.Background(), reg, &buf,
		registry.HistoryFilter{Service: "worker", Limit: 50}, false); err != nil {
		t.Fatalf("RunHistory: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "v1.0.0") || strings.Contains(got, "v2.0.0") || strings.Contains(got, "v2.0.1") {
		t.Errorf("expected only worker rows, found api version:\n%s", got)
	}
	if !strings.Contains(got, "v1.0.1") {
		t.Errorf("expected worker v1.0.1, got:\n%s", got)
	}
}

// TestHistoryClusterAndServiceFilter verifies both filters AND together.
func TestHistoryClusterAndServiceFilter(t *testing.T) {
	reg := newTempRegistry(t)
	seedHistory(t, reg)

	var buf bytes.Buffer
	if err := cmd.RunHistory(context.Background(), reg, &buf,
		registry.HistoryFilter{ClusterName: "beta", Service: "api", Limit: 50}, false); err != nil {
		t.Fatalf("RunHistory: %v", err)
	}
	got := buf.String()
	// beta+api should match v2.0.0 and v2.0.1 only.
	for _, v := range []string{"v2.0.0", "v2.0.1"} {
		if !strings.Contains(got, v) {
			t.Errorf("expected %s in output, got:\n%s", v, got)
		}
	}
	for _, v := range []string{"v1.0.0", "v1.0.1"} {
		if strings.Contains(got, v) {
			t.Errorf("did not expect %s in beta+api output:\n%s", v, got)
		}
	}
}

// TestHistoryLimit verifies --limit caps the row count.
func TestHistoryLimit(t *testing.T) {
	reg := newTempRegistry(t)
	seedHistory(t, reg)

	var buf bytes.Buffer
	if err := cmd.RunHistory(context.Background(), reg, &buf,
		registry.HistoryFilter{Limit: 2}, true); err != nil {
		t.Fatalf("RunHistory: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, buf.String())
	}
	if len(rows) != 2 {
		t.Errorf("expected 2 rows, got %d: %+v", len(rows), rows)
	}
	// Reverse-chronological: newest two are v2.0.1 then v2.0.0.
	if rows[0]["version"] != "v2.0.1" || rows[1]["version"] != "v2.0.0" {
		t.Errorf("unexpected limit order: %+v", rows)
	}
}

// TestHistoryJSON verifies that --json emits structured rows with the
// documented field names and millisecond durations.
func TestHistoryJSON(t *testing.T) {
	reg := newTempRegistry(t)
	seedHistory(t, reg)

	var buf bytes.Buffer
	if err := cmd.RunHistory(context.Background(), reg, &buf,
		registry.HistoryFilter{Limit: 50}, true); err != nil {
		t.Fatalf("RunHistory: %v", err)
	}

	var rows []struct {
		When       string `json:"when"`
		Cluster    string `json:"cluster"`
		Service    string `json:"service"`
		Version    string `json:"version"`
		Status     string `json:"status"`
		DurationMs int64  `json:"duration_ms"`
		Error      string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, buf.String())
	}
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(rows))
	}
	// Newest first.
	if rows[0].Version != "v2.0.1" {
		t.Errorf("expected newest row first, got %s", rows[0].Version)
	}
	if rows[0].DurationMs != 999 {
		t.Errorf("expected raw ms in JSON, got %d", rows[0].DurationMs)
	}
	// The failed alpha/worker row carries an error message.
	var failedFound bool
	for _, r := range rows {
		if r.Status == "failed" {
			failedFound = true
			if r.Error == "" {
				t.Errorf("expected non-empty error on failed row, got: %+v", r)
			}
		}
	}
	if !failedFound {
		t.Errorf("expected at least one failed row in output: %+v", rows)
	}
}

// TestHistoryJSONEmpty verifies --json on an empty registry produces "[]".
func TestHistoryJSONEmpty(t *testing.T) {
	reg := newTempRegistry(t)

	var buf bytes.Buffer
	if err := cmd.RunHistory(context.Background(), reg, &buf,
		registry.HistoryFilter{Limit: 50}, true); err != nil {
		t.Fatalf("RunHistory: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, buf.String())
	}
	if len(rows) != 0 {
		t.Errorf("expected empty array, got %d rows", len(rows))
	}
}

// TestHistoryErrorTruncation verifies the ERROR column is truncated to 60
// runes with an ellipsis suffix and that multi-byte runes are not split.
func TestHistoryErrorTruncation(t *testing.T) {
	reg := newTempRegistry(t)
	ctx := context.Background()

	// 80-rune ASCII error, should truncate to 57 chars + "...".
	longASCII := strings.Repeat("x", 80)
	if err := reg.AppendHistory(ctx, registry.DeploymentHistoryEntry{
		ClusterName: "alpha", Service: "api", Version: "v1",
		AttemptedAt: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC),
		Status:      registry.StatusFailed, Error: longASCII,
	}); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	// 80-rune string composed of 3-byte UTF-8 runes; truncation must not
	// split a rune. "日" is 3 bytes in UTF-8.
	longUTF := strings.Repeat("日", 80)
	if err := reg.AppendHistory(ctx, registry.DeploymentHistoryEntry{
		ClusterName: "alpha", Service: "worker", Version: "v1",
		AttemptedAt: time.Date(2026, 4, 20, 12, 1, 0, 0, time.UTC),
		Status:      registry.StatusFailed, Error: longUTF,
	}); err != nil {
		t.Fatalf("AppendHistory utf: %v", err)
	}

	var buf bytes.Buffer
	if err := cmd.RunHistory(ctx, reg, &buf, registry.HistoryFilter{Limit: 50}, false); err != nil {
		t.Fatalf("RunHistory: %v", err)
	}
	got := buf.String()

	// ASCII: full 80-x string must not survive intact; truncated form must.
	if strings.Contains(got, longASCII) {
		t.Errorf("expected long ASCII error to be truncated, got:\n%s", got)
	}
	if !strings.Contains(got, strings.Repeat("x", 57)+"...") {
		t.Errorf("expected ASCII truncation suffix '...', got:\n%s", got)
	}

	// UTF-8: must contain a valid rune-aligned prefix with the ellipsis.
	wantUTF := strings.Repeat("日", 57) + "..."
	if !strings.Contains(got, wantUTF) {
		t.Errorf("expected UTF-8 rune-aware truncation %q, got:\n%s", wantUTF, got)
	}
	// And the full untruncated UTF-8 must not appear.
	if strings.Contains(got, longUTF) {
		t.Errorf("expected long UTF-8 error to be truncated, got:\n%s", got)
	}
}
