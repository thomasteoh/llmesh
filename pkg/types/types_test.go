package types_test

import (
	"encoding/json"
	"testing"

	"llmesh/pkg/types"
)

func TestReleaseMsg_JSON(t *testing.T) {
	msg := types.ReleaseMsg{
		Type:      "release",
		RequestID: "req-123",
		Reason:    "model_failed",
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got types.ReleaseMsg
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != "release" || got.RequestID != "req-123" || got.Reason != "model_failed" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}
