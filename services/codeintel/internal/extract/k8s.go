package extract

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

var (
	reK8sName     = regexp.MustCompile(`(?m)^\s{2}name:\s*["']?([\w.-]+)`)
	reK8sSchedule = regexp.MustCompile(`(?m)^\s*schedule:\s*["']?([^"'\n#]+)`)
)

type k8sCron struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Schedule    string `yaml:"schedule"`
		JobTemplate struct {
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Env []struct {
								Name  string `yaml:"name"`
								Value string `yaml:"value"`
							} `yaml:"env"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		} `yaml:"jobTemplate"`
	} `yaml:"spec"`
}

// extractK8sCron finds Kubernetes CronJobs; the service is TROOPER_SERVICE from the container env when set.
// Helm templates ({{ … }}) that do not parse as YAML fall back to a regex over each document.
func extractK8sCron(f srcFile) []model.Item {
	if !bytes.Contains(f.src, []byte("CronJob")) {
		return nil
	}
	var out []model.Item
	docs := bytes.Split(f.src, []byte("\n---"))
	lineBase := 1
	for _, doc := range docs {
		docLine := lineBase
		lineBase += bytes.Count(doc, []byte("\n")) + 1
		if !bytes.Contains(doc, []byte("CronJob")) {
			continue
		}
		var c k8sCron
		dec := yaml.NewDecoder(bytes.NewReader(doc))
		err := dec.Decode(&c)
		if err != nil && !errors.Is(err, io.EOF) {
			// Helm template: regex fallback.
			s := string(doc)
			if !strings.Contains(s, "kind: CronJob") {
				continue
			}
			name, sched := reK8sName.FindStringSubmatch(s), reK8sSchedule.FindStringSubmatch(s)
			if name != nil && sched != nil {
				out = append(out, model.Item{Kind: model.KindCron, Key: name[1], Source: strings.TrimSpace(sched[1]), File: f.rel,
					Line: docLine, Framework: "k8s-cronjob", Language: "yaml", Snippet: s})
			}
			continue
		}
		if c.Kind != "CronJob" || c.Metadata.Name == "" {
			continue
		}
		it := model.Item{Kind: model.KindCron, Key: c.Metadata.Name, Source: c.Spec.Schedule, File: f.rel, Line: docLine,
			Framework: "k8s-cronjob", Language: "yaml", Snippet: strings.TrimSpace(string(doc))}
		for _, ct := range c.Spec.JobTemplate.Spec.Template.Spec.Containers {
			for _, e := range ct.Env {
				if e.Name == "TROOPER_SERVICE" && e.Value != "" {
					it.Service = e.Value
				}
			}
		}
		out = append(out, it)
	}
	return out
}
