package streamreply

import (
	"strings"
	"testing"
)

func TestChatEOFRequiresEveryChoiceAndCompleteFrame(t *testing.T) {
	const one = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n"
	got, err := AssembleChat(strings.NewReader(one), "text/event-stream")
	if err != nil || !strings.Contains(string(got), "answer") || strings.Contains(string(got), `"usage"`) {
		t.Fatalf("%s, %v", got, err)
	}
	for _, bad := range []string{
		strings.TrimSuffix(one, "\n"),
		strings.Replace(one, `"stop"`, "null", 1),
		strings.Replace(one, `"stop"`, `""`, 1),
		strings.Replace(one, `"stop"`, "false", 1),
		one + "data: {\"choices\":[{\"index\":1,\"delta\":{\"content\":\"unfinished\"},\"finish_reason\":null}]}\n\n",
		one + "data: {\"error\":{\"message\":\"failed\"}}\n\n",
	} {
		if _, err := AssembleChat(strings.NewReader(bad), "text/event-stream"); err == nil {
			t.Fatal("incomplete response was accepted")
		}
	}
}
