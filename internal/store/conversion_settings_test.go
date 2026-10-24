package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestConversionReasoningPolicyPersistsAndRejectsInvalidWrites(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	initial, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	if initial.ReasoningPolicy != ReasoningPreserve {
		t.Fatalf("new store policy = %q", initial.ReasoningPolicy)
	}
	for _, policy := range []string{ReasoningAvailability, ReasoningPreserve, ""} {
		current, err := s.GetSettings()
		if err != nil {
			t.Fatal(err)
		}
		current.ReasoningPolicy = policy
		if err := s.SaveSettings(current); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.GetSettings()
		if err != nil {
			t.Fatal(err)
		}
		want := policy
		if want == "" {
			want = ReasoningPreserve
		}
		if got.ReasoningPolicy != want || got.GatewayKey != initial.GatewayKey || got.MaxAttempts != initial.MaxAttempts {
			t.Fatalf("reopened settings did not retain the accepted policy and other values: policy=%q max_attempts=%d", got.ReasoningPolicy, got.MaxAttempts)
		}
	}
	before, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	for _, policy := range []string{"drop_all", "PRESERVE", " availability "} {
		t.Run(policy, func(t *testing.T) {
			draft := before
			draft.ReasoningPolicy = policy
			draft.MaxAttempts++
			if err := s.SaveSettings(draft); err == nil {
				t.Fatal("invalid policy accepted")
			}
			after, err := s.GetSettings()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatal("rejected update overwrote previously accepted settings")
			}
			patch, err := json.Marshal(map[string]any{"reasoning_policy": policy, "max_attempts": before.MaxAttempts + 1})
			if err != nil {
				t.Fatal(err)
			}
			result, err := s.PatchRuntimeSettings(EntityVersion(SafeSettings(before)), patch)
			var validation *ConfigurationError
			if !errors.As(err, &validation) {
				t.Fatalf("invalid patch did not return a known validation refusal: %v", err)
			}
			if !reflect.DeepEqual(result, Settings{}) {
				t.Fatal("rejected patch returned an unsaved draft as accepted settings")
			}
		})
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatal("rejected update was persisted across reopening")
	}
}

func TestConversionUsageDiagnosticAndRequestIsolationPersist(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	requestID := "synthetic/request+7'quoted"
	diagnostic := json.RawMessage(`{"code":"reasoning_rejected","source":"upstream","phase":"upstream_response","message":"思考状态被拒绝","facts":["未交付答案"],"possible_causes":["目标可能已切换"],"steps":[{"action":"核对目标","expected":"比较结果"}],"actions_taken":["未自动重试"],"settings_path":"#/settings?section=conversion","request_id":"synthetic-diagnostic"}`)
	for i, id := range []string{requestID, requestID, requestID, requestID + "-other", "different-request", ""} {
		u := Usage{Time: time.Now(), RequestID: id, ChannelID: int64(i + 1), Model: "synthetic", Status: 200, Diagnostic: diagnostic}
		if i == 0 {
			u.Status = 400
		}
		if i == 2 {
			u.NotSent = true
		}
		if err := s.AddUsage(u); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListUsage(UsageFilter{RequestID: requestID})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("request filter returned %d rows, want all 3 attempts", len(rows))
	}
	for _, row := range rows {
		if row.RequestID != requestID {
			t.Fatal("another request crossed the filter")
		}
		var got, want any
		if err := json.Unmarshal(row.Diagnostic, &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(diagnostic, &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatal("diagnostic did not survive storage and reopening")
		}
		detail, err := s.GetUsageByID(context.Background(), row.ID)
		if err != nil || detail.RequestID != requestID || !reflect.DeepEqual(detail.Diagnostic, row.Diagnostic) {
			t.Fatalf("detail lost correlation or diagnostic: %v", err)
		}
	}
	page, err := s.ListUsagePage(context.Background(), UsagePageRequest{Filter: UsageFilter{RequestID: requestID}, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for {
		for _, row := range page.Items {
			if row.RequestID != requestID || seen[row.ID] {
				t.Fatal("pagination mixed requests or repeated a row")
			}
			seen[row.ID] = true
		}
		if !page.HasMore {
			break
		}
		if _, err := s.ListUsagePage(context.Background(), UsagePageRequest{Filter: UsageFilter{RequestID: "different-request"}, Cursor: page.NextCursor, Limit: 1}); err == nil {
			t.Fatal("correlated cursor accepted a different request filter")
		}
		page, err = s.ListUsagePage(context.Background(), UsagePageRequest{Filter: UsageFilter{RequestID: requestID}, Cursor: page.NextCursor, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("paged attempts = %d, want 3", len(seen))
	}
	missing, err := s.ListUsage(UsageFilter{RequestID: "absent-request"})
	if err != nil || len(missing) != 0 {
		t.Fatalf("missing correlation selected unrelated logs: count=%d err=%v", len(missing), err)
	}
}
