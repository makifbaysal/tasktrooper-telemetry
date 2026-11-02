package store

import (
	"strings"
	"testing"

	"github.com/tasktrooper/telemetry/services/api/internal/model"
)

func TestUserTitle(t *testing.T) {
	tests := []struct {
		in   model.AssistantUserContent
		want string
	}{
		{model.AssistantUserContent{Text: "  why\nslow? "}, "why slow?"},
		{model.AssistantUserContent{Text: "Why is slow?", Refs: []model.AssistantRef{{Label: "tt-api"}}}, "Why is slow? · tt-api"},
		{model.AssistantUserContent{Attachments: []model.AssistantAttachmentRef{{ID: "x"}}}, "Image"},
		{model.AssistantUserContent{Text: strings.Repeat("a", 200)}, strings.Repeat("a", 79) + "…"},
	}
	for _, tc := range tests {
		if got := UserTitle(tc.in); got != tc.want {
			t.Errorf("got %q want %q", got, tc.want)
		}
	}
}
