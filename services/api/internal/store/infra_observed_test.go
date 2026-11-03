package store

import (
	"reflect"
	"testing"
)

func TestMergeObservedDBs(t *testing.T) {
	type got struct{ slug, name, engine string }
	for _, tc := range []struct {
		name  string
		metas []dbMeta
		obs   []observedDB
		deps  []string
		want  []got
	}{
		{"demo agent.yaml: unique targets per engine",
			nil,
			[]observedDB{{"tt-postgres", "postgresql"}, {"tt-redis", "redis"}},
			[]string{"postgresql · trooper", "redis · redis", "HTTP · x"},
			[]got{{"tt-redis", "redis", "redis"}, {"tt-postgres", "trooper", "postgres"}}},
		{"registered row wins, its name is not reused",
			[]dbMeta{{slug: "orders", name: "orders-db", engine: "postgres"}},
			[]observedDB{{"orders", "postgresql"}, {"tt-postgres", "postgresql"}},
			[]string{"postgresql · orders-db", "postgresql · trooper"},
			[]got{{"orders", "orders-db", "postgres"}, {"tt-postgres", "trooper", "postgres"}}},
		{"ambiguous: two targets → name = id",
			nil,
			[]observedDB{{"pg-1", "postgresql"}},
			[]string{"postgresql · a", "postgresql · b"},
			[]got{{"pg-1", "pg-1", "postgres"}}},
		{"id equals a target",
			nil,
			[]observedDB{{"a", "postgresql"}, {"pg-2", "postgresql"}},
			[]string{"postgresql · a", "postgresql · b"},
			[]got{{"a", "a", "postgres"}, {"pg-2", "b", "postgres"}}},
		{"clickhouse target (tt-clickhouse, db telemetry)",
			nil,
			[]observedDB{{"tt-clickhouse", "clickhouse"}},
			[]string{"clickhouse · telemetry"},
			[]got{{"tt-clickhouse", "telemetry", "clickhouse"}}},
		{"no metrics → registered only",
			[]dbMeta{{slug: "x", name: "x", engine: "mysql"}}, nil, nil,
			[]got{{"x", "x", "mysql"}}},
	} {
		var out []got
		for _, m := range MergeObservedDBs(tc.metas, tc.obs, tc.deps) {
			out = append(out, got{m.slug, m.name, m.engine})
			if m.observed && m.collector != "agent" {
				t.Errorf("%s: observed collector %q", tc.name, m.collector)
			}
		}
		if !reflect.DeepEqual(out, tc.want) {
			t.Errorf("%s: %v want %v", tc.name, out, tc.want)
		}
	}
}

func TestMergeObservedClusters(t *testing.T) {
	type got struct {
		slug, kind string
		observed   bool
	}
	for _, tc := range []struct {
		name  string
		metas []clusterMeta
		obs   []observedCluster
		deps  []string
		want  []got
	}{
		{"kind from topic dependency",
			nil, []observedCluster{{id: "tt-redpanda", nodes: []string{"redpanda-0"}, topics: []string{"tt.spans"}}},
			[]string{"kafka · tt.spans"}, []got{{"tt-redpanda", "kafka", true}}},
		{"rabbitmq node name",
			nil, []observedCluster{{id: "mq", nodes: []string{"rabbit@mq-0"}}}, nil, []got{{"mq", "rabbitmq", true}}},
		{"sqs from dependency",
			nil, []observedCluster{{id: "q", topics: []string{"jobs"}}}, []string{"aws_sqs · jobs"}, []got{{"q", "sqs", true}}},
		{"default kafka; registered wins",
			[]clusterMeta{{slug: "main", name: "main", kind: "rabbitmq"}},
			[]observedCluster{{id: "main"}, {id: "other"}}, nil,
			[]got{{"main", "rabbitmq", false}, {"other", "kafka", true}}},
	} {
		var out []got
		for _, m := range MergeObservedClusters(tc.metas, tc.obs, tc.deps) {
			out = append(out, got{m.slug, m.kind, m.observed})
		}
		if !reflect.DeepEqual(out, tc.want) {
			t.Errorf("%s: %v want %v", tc.name, out, tc.want)
		}
	}
}

func TestEngineOf(t *testing.T) {
	for in, want := range map[string]string{"postgresql": "postgres", "PostgreSQL": "postgres", "redis": "redis", "mongodb": "mongodb", "": ""} {
		if got := EngineOf(in); got != want {
			t.Errorf("EngineOf(%q) = %q want %q", in, got, want)
		}
	}
}
