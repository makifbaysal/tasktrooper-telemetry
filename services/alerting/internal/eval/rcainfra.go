package eval

import (
	"context"
	"math"
	"time"

	"github.com/tasktrooper/telemetry/services/alerting/internal/rca"
)

// InfraSignals reads the pods of service (k8s_pods_1m, by trooper.service) and the hosts running it (containers_1m
// hosts + the pods' nodes, hosts_1m) over the RCA "current" window (last 15 minutes).
func InfraSignals(ctx context.Context, ch Querier, org, project uint32, service string, now time.Time) ([]rca.PodSignal, []rca.HostSignal, error) {
	to := now.Truncate(time.Minute)
	from := to.Add(-RCACurrent)
	base := func() *where {
		return (&where{parts: []string{"org_id = ?", "project_id = ?"}, args: []any{org, project}}).between("minute", from, to)
	}
	w := base().eq("service", service)
	rows, err := ch.Query(ctx, `SELECT cluster, namespace, pod, any(node), groupUniqArrayIf(reason, reason != ''),
		if(countIf(isFinite(r)) > 0, maxIf(r, isFinite(r)) - minIf(r, isFinite(r)), 0)
		FROM (SELECT cluster, namespace, pod, pod_uid, minute, any(node) AS node, anyLast(reason) AS reason, max(restarts) AS r
			FROM telemetry.k8s_pods_1m WHERE `+w.sql()+` GROUP BY cluster, namespace, pod, pod_uid, minute)
		GROUP BY cluster, namespace, pod, pod_uid`, w.args...)
	if err != nil {
		return nil, nil, err
	}
	var pods []rca.PodSignal
	for rows.Next() {
		var p rca.PodSignal
		if err := rows.Scan(&p.Cluster, &p.Namespace, &p.Pod, &p.Node, &p.Reasons, &p.RestartIncrease); err != nil {
			rows.Close()
			return nil, nil, err
		}
		pods = append(pods, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	cw, pw := base().eq("service", service), base().eq("service", service)
	hw := base().add("(host IN (SELECT DISTINCT host FROM telemetry.containers_1m WHERE "+cw.sql()+
		") OR (node != '' AND node IN (SELECT DISTINCT node FROM telemetry.k8s_pods_1m WHERE "+pw.sql()+")))", append(cw.args, pw.args...)...)
	rows, err = ch.Query(ctx, `SELECT host, avg(c) FROM (SELECT host, minute, max(cpu) AS c FROM telemetry.hosts_1m WHERE `+hw.sql()+
		` GROUP BY host, minute) WHERE isFinite(c) GROUP BY host`, hw.args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var hosts []rca.HostSignal
	for rows.Next() {
		var h rca.HostSignal
		if err := rows.Scan(&h.Host, &h.CPU); err != nil {
			return nil, nil, err
		}
		if !math.IsNaN(h.CPU) {
			hosts = append(hosts, h)
		}
	}
	return pods, hosts, rows.Err()
}
