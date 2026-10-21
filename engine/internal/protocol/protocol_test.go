package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRequestRoundTripPreservesCloneAndArguments(t *testing.T) {
	input := `{"action":"machine.clone","name":"source","destination":"copy","configuration":{"cpus":2},"arguments":["printf","a\nb"]}`
	var request Request
	if err := Decode(strings.NewReader(input), &request); err != nil {
		t.Fatal(err)
	}
	output, _ := json.Marshal(request)
	var a, b any
	json.Unmarshal([]byte(input), &a)
	json.Unmarshal(output, &b)
	normalizedA, _ := json.Marshal(a)
	normalizedB, _ := json.Marshal(b)
	if string(normalizedA) != string(normalizedB) {
		t.Fatalf("request changed: %s", output)
	}
}

func TestDecodeRejectsUnboundedOrMultipleMessages(t *testing.T) {
	for _, value := range []string{"{}", "{} {}", strings.Repeat(" ", MaxMessage+1)} {
		var request Request
		err := Decode(strings.NewReader(value), &request)
		if value == "{}" {
			if err == nil {
				err = request.Validate()
			}
		}
		if err == nil {
			t.Fatalf("accepted invalid request length %d", len(value))
		}
	}
}
