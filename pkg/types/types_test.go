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

func TestModalityForContentType(t *testing.T) {
	cases := map[string]string{
		"text":        "",
		"image_url":   types.ModalityVision,
		"input_image": types.ModalityVision,
		"image":       types.ModalityVision,
		"input_audio": types.ModalityAudio,
		"audio":       types.ModalityAudio,
		"video_url":   types.ModalityVideo,
		"tool_result": "",
		"":            "",
	}
	for in, want := range cases {
		if got := types.ModalityForContentType(in); got != want {
			t.Errorf("ModalityForContentType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestModalitiesCompatible(t *testing.T) {
	cases := []struct {
		name       string
		advertised []string
		required   []string
		want       bool
	}{
		{"no requirement", []string{"text"}, nil, true},
		{"unknown advertised is never excluded", nil, []string{"vision"}, true},
		{"capable", []string{"text", "vision"}, []string{"vision"}, true},
		{"known text-only rejects vision", []string{"text"}, []string{"vision"}, false},
		{"missing one of several", []string{"text", "vision"}, []string{"vision", "audio"}, false},
		{"all present", []string{"text", "vision", "audio"}, []string{"vision", "audio"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := types.ModalitiesCompatible(tc.advertised, tc.required); got != tc.want {
				t.Errorf("ModalitiesCompatible(%v, %v) = %v, want %v", tc.advertised, tc.required, got, tc.want)
			}
		})
	}
}
