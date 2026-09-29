package localapi

import (
	"encoding/json"
	"testing"
)

func TestWithChatTemplate(t *testing.T) {
	body := []byte(`{"model":"m","messages":[]}`)
	if out, _ := withChatTemplate(body, ""); string(out) != string(body) {
		t.Errorf("no template configured, body changed: %s", out)
	}
	out, err := withChatTemplate(body, "{{custom}}")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(out, &got)
	if got["chat_template"] != "{{custom}}" || got["model"] != "m" {
		t.Errorf("configured template not applied: %s", out)
	}
	own := []byte(`{"model":"m","chat_template":"{{mine}}"}`)
	if out, _ := withChatTemplate(own, "{{custom}}"); string(out) != string(own) {
		t.Errorf("the caller's own template was replaced: %s", out)
	}
}
