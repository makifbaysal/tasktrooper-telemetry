package ctl

import "testing"

func TestJobEnvTrace(t *testing.T) {
	tp := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	cases := []struct {
		name  string
		trace map[string]string
		want  string
		has   bool
	}{
		{"self telemetry off", nil, "", false},
		{"traceparent", map[string]string{"TRACEPARENT": tp}, tp, true},
		{"empty value dropped", map[string]string{"TRACEPARENT": ""}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := Job{RunID: 7, Region: "eu", Share: 1, Trace: c.trace}.Env()
			got, ok := env["TRACEPARENT"]
			if ok != c.has || got != c.want {
				t.Fatalf("TRACEPARENT = %q (%v), want %q (%v)", got, ok, c.want, c.has)
			}
			if env["TT_LOADTEST_RUN_ID"] != "7" {
				t.Fatalf("job env lost: %v", env)
			}
			// the k8s Job renders the same env
			found := ""
			job := RenderJob(K8sConfig{Image: "img", Namespace: "ns"}, Job{RunID: 7, Region: "eu", Trace: c.trace})
			pod := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			for _, e := range pod["containers"].([]any)[0].(map[string]any)["env"].([]any) {
				if m := e.(map[string]any); m["name"] == "TRACEPARENT" {
					found = m["value"].(string)
				}
			}
			if found != c.want {
				t.Fatalf("k8s TRACEPARENT = %q, want %q", found, c.want)
			}
		})
	}
}
