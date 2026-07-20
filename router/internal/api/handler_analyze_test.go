package api

import (
	"encoding/json"
	"reflect"
	"testing"

	"llmesh/pkg/types"
)

func msg(role, content string) types.Message {
	return types.Message{Role: role, Content: json.RawMessage(content)}
}

func TestAnalyzeMessages(t *testing.T) {
	t.Run("plain text", func(t *testing.T) {
		req := &types.InferenceRequest{Messages: []types.Message{msg("user", `"hello there friend"`)}}
		wc, mods := analyzeMessages(req)
		if wc != 3 {
			t.Errorf("word count = %d, want 3", wc)
		}
		if len(mods) != 0 {
			t.Errorf("modalities = %v, want none", mods)
		}
	})

	t.Run("image part adds vision and a token allowance", func(t *testing.T) {
		content := `[{"type":"text","text":"describe this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]`
		req := &types.InferenceRequest{Messages: []types.Message{msg("user", content)}}
		wc, mods := analyzeMessages(req)
		if want := 2 + perImagePromptWords; wc != want {
			t.Errorf("word count = %d, want %d (2 text + image allowance)", wc, want)
		}
		if !reflect.DeepEqual(mods, []string{types.ModalityVision}) {
			t.Errorf("modalities = %v, want [vision]", mods)
		}
	})

	t.Run("audio part adds audio modality", func(t *testing.T) {
		content := `[{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]`
		req := &types.InferenceRequest{Messages: []types.Message{msg("user", content)}}
		wc, mods := analyzeMessages(req)
		if wc != perAudioPromptWords {
			t.Errorf("word count = %d, want %d", wc, perAudioPromptWords)
		}
		if !reflect.DeepEqual(mods, []string{types.ModalityAudio}) {
			t.Errorf("modalities = %v, want [audio]", mods)
		}
	})

	t.Run("modalities are sorted and de-duplicated", func(t *testing.T) {
		content := `[{"type":"input_audio"},{"type":"image_url"},{"type":"image_url"}]`
		req := &types.InferenceRequest{Messages: []types.Message{msg("user", content)}}
		_, mods := analyzeMessages(req)
		if !reflect.DeepEqual(mods, []string{types.ModalityAudio, types.ModalityVision}) {
			t.Errorf("modalities = %v, want [audio vision]", mods)
		}
	})
}

func TestMaxRequestBytes(t *testing.T) {
	cases := []struct {
		name string
		set  int
		want int64
	}{
		{"unset uses default", 0, defaultMaxRequestBytes},
		{"negative uses default", -5, defaultMaxRequestBytes},
		{"explicit within range", 4 << 20, 4 << 20},
		{"above ceiling is clamped", 64 << 20, maxRequestBytesCeiling},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{MaxRequestBytes: tc.set}
			if got := h.maxRequestBytes(); got != tc.want {
				t.Errorf("maxRequestBytes() = %d, want %d", got, tc.want)
			}
		})
	}
}
