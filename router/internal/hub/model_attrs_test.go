package hub

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func registerAs(t *testing.T, conn *websocket.Conn, kind, version string, models ...map[string]any) {
	t.Helper()
	msg, _ := json.Marshal(map[string]any{"type": "register", "models": models, "max_concurrent": 1, "kind": kind, "version": version})
	if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
}

func TestModelAttrs(t *testing.T) {
	h := New(slog.Default())
	a := dialHub(t, h, "a", "alice", "t1")
	registerAs(t, a, "llama.cpp", "1.0", map[string]any{"name": "local", "context_size": 8192, "modalities": []string{"vision"}},
		map[string]any{"name": "both", "context_size": 4096})
	b := dialHub(t, h, "b", "bob", "t2")
	registerAs(t, b, "shim", "1.0", map[string]any{"name": "paid", "context_size": 128000},
		map[string]any{"name": "both", "context_size": 32000})
	c := dialHub(t, h, "c", "carol", "t3")
	registerAs(t, c, "", "0.9", map[string]any{"name": "legacy"})
	d := dialHub(t, h, "d", "up", "t4")
	registerAs(t, d, "", "router/1.0", map[string]any{"name": "remote"})

	for model, want := range map[string]map[string]any{
		"local":  {"served_by_kind": "llama.cpp", "context_size": float64(8192)},
		"paid":   {"served_by_kind": "shim", "context_size": float64(128000)},
		"both":   {"served_by_kind": "mixed", "context_size": float64(32000)},
		"legacy": {},
		"remote": {"served_by_kind": "router"},
	} {
		got := h.ModelAttrs(model)
		if got["served_by_kind"] != want["served_by_kind"] || got["context_size"] != want["context_size"] {
			t.Errorf("%s: %v, want %v", model, got, want)
		}
	}
	if mods, _ := h.ModelAttrs("local")["modalities"].([]string); len(mods) != 1 || mods[0] != "vision" {
		t.Errorf("modalities: %v", h.ModelAttrs("local")["modalities"])
	}
}
