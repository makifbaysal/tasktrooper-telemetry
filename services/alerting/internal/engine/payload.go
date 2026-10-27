package engine

import (
	"context"
	"time"

	"github.com/tasktrooper/telemetry/services/alerting/internal/model"
	"github.com/tasktrooper/telemetry/services/alerting/internal/rca"
)

// deployWindow: deploys this recent are attached to firing notifications (contracts §17 "deploy context").
const deployWindow = time.Hour

// addCause sets summary and rootCause {summary, confidence} (Slack: "Likely cause: … (NN% confidence)").
func addCause(p map[string]any, summary string, confidence *float64) {
	if summary == "" {
		return
	}
	p["summary"] = summary
	rc := map[string]any{"summary": summary}
	if confidence != nil {
		rc["confidence"] = *confidence
	}
	p["rootCause"] = rc
}

func addRCA(p map[string]any, r *rca.Result) {
	if r == nil {
		return
	}
	c := r.Confidence
	addCause(p, r.Summary, &c)
}

// addMonitorRefs adds source / codeRef (alert-as-code monitors link to their definition).
func addMonitorRefs(p map[string]any, m *model.Monitor) {
	if m.Source != "" {
		p["source"] = m.Source
	}
	if m.CodeRef != "" {
		p["codeRef"] = m.CodeRef
	}
}

// deployContext returns the newest deploy of service within the last hour as a payload map, or nil.
func (e *Engine) deployContext(ctx context.Context, projectID uint32, service string, now time.Time) map[string]any {
	d, err := e.st.RecentDeploy(ctx, int64(projectID), service, now, deployWindow)
	if err != nil {
		e.log.Warn("deploy context lookup failed", "service", service, "err", err)
		return nil
	}
	if d == nil {
		return nil
	}
	out := map[string]any{"version": d.Version, "source": d.Source, "at": d.TS.UnixMilli(), "env": d.Env}
	for k, v := range map[string]string{"url": d.URL, "commitMessage": d.CommitMessage, "author": d.Author} {
		if v != "" {
			out[k] = v
		}
	}
	return out
}
