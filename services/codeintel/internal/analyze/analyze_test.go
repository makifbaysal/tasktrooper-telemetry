package analyze

import (
	"testing"
	"time"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

func TestIncremental(t *testing.T) {
	now := time.Now()
	analyzed := model.Repo{AnalyzedAt: &now, LastAnalyzedSHA: "abc"}
	big := make([]string, 201)
	cases := []struct {
		name    string
		repo    model.Repo
		pending []string
		want    bool
	}{
		{"changed files", analyzed, []string{"a.go"}, true},
		{"no list → full", analyzed, nil, false},
		{"full marker", analyzed, []string{"a.go", "*"}, false},
		{"too many", analyzed, big, false},
		{"never analyzed", model.Repo{}, []string{"a.go"}, false},
	}
	for _, c := range cases {
		if got := Incremental(c.repo, c.pending); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}
