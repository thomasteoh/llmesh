// router/internal/reqopt/reqopt.go
//
// Request shaping applied to inbound inference requests before dispatch.
// Each transformation is gated by a toggle in types.RequestOptimization so
// the default (all-off) path leaves requests byte-for-byte unchanged.
package reqopt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"llmesh/pkg/types"
)

// Clean mutates req in place according to opts. It is a no-op when the relevant
// toggles are off. Only plain-string message content is rewritten; structured
// content (tool_use/tool_result/multimodal arrays and objects) is left intact
// so cleaning can never corrupt a structured block.
func Clean(req *types.InferenceRequest, opts types.RequestOptimization) {
	if opts.ClampParams {
		clampParams(req)
	}
	if !opts.CleanRequests {
		return
	}
	cleaned := req.Messages[:0:0] // new backing array; preserve original until we know it's non-empty
	for _, m := range req.Messages {
		if s, ok := plainString(m.Content); ok {
			s = trimString(s, opts.CleanAggressive)
			// Drop messages that carry neither text nor a tool payload.
			if s == "" && len(m.ToolCalls) == 0 && m.ToolCallID == "" {
				continue
			}
			b, err := json.Marshal(s)
			if err == nil {
				m.Content = b
			}
		} else if isEmptyContent(m.Content) && len(m.ToolCalls) == 0 && m.ToolCallID == "" {
			continue
		}
		cleaned = append(cleaned, m)
	}
	// Never strip a request down to nothing — if every message was empty, keep
	// the original so the backend returns a meaningful error rather than us
	// silently sending an empty conversation.
	if len(cleaned) > 0 {
		req.Messages = cleaned
	}
}

// clampParams brings out-of-range sampling parameters into valid bounds.
// Unset parameters (nil pointers) are left untouched so the backend default
// applies; only explicitly-provided values are clamped.
func clampParams(req *types.InferenceRequest) {
	if req.Temperature != nil {
		if *req.Temperature < 0 {
			*req.Temperature = 0
		} else if *req.Temperature > 2 {
			*req.Temperature = 2
		}
	}
	if req.TopP != nil {
		if *req.TopP < 0 {
			*req.TopP = 0
		} else if *req.TopP > 1 {
			*req.TopP = 1
		}
	}
}

// PrefixKey returns a stable key identifying a request's shared leading prefix:
// the model plus the system message and first user turn. Requests belonging to
// the same conversation share these opening turns and therefore the same key,
// letting the scheduler route them to one client and keep its prompt KV cache
// warm. Returns "" when there is nothing stable to key on.
func PrefixKey(req *types.InferenceRequest) string {
	if len(req.Messages) == 0 {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(req.Model))
	h.Write([]byte{0})
	// Include the system message (if first) and the first user message. These
	// form the stable head of a multi-turn conversation.
	for _, m := range req.Messages {
		h.Write([]byte(m.Role))
		h.Write([]byte{0})
		h.Write(m.Content)
		h.Write([]byte{0})
		if m.Role == "user" {
			break
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// plainString reports whether raw is a JSON string literal and returns its value.
func plainString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// isEmptyContent reports whether raw is JSON null, an empty array, or absent.
func isEmptyContent(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null" || s == "[]"
}

// trimString trims surrounding whitespace. When aggressive, it also collapses
// interior runs of spaces/tabs to a single space on each line while preserving
// line breaks.
func trimString(s string, aggressive bool) string {
	s = strings.TrimSpace(s)
	if !aggressive || s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(collapseSpaces(line), " \t")
	}
	return strings.Join(lines, "\n")
}

// collapseSpaces replaces runs of spaces/tabs with a single space.
func collapseSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' {
			if !prevSpace {
				b.WriteByte(' ')
			}
			prevSpace = true
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	return b.String()
}
