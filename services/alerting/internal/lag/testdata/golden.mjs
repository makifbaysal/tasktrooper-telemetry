// Verbatim formulas from apps/web/src/lib/lagAlerts.ts (ruleSeries/evaluate) with stepMin injected.
const stepMin = 1;
function series(metric, lag, rate, maxPart, reb) {
  switch (metric) {
    case 'lag': return lag;
    case 'time_behind': return lag.map((v, i) => v / (rate[i] || 1));
    case 'partition_lag': return maxPart;
    case 'growing': {
      const w = 5;
      const smooth = lag.map((_, i) => { const xs = lag.slice(Math.max(0, i - w + 1), i + 1); return xs.reduce((x, y) => x + y, 0) / xs.length; });
      const k = 4;
      return smooth.map((v, i) => (i < k + w ? 0 : (v - smooth[i - k]) / (k * stepMin)));
    }
    case 'not_drained': {
      const peak = Math.max(...lag) || 1;
      let since = 0;
      return lag.map((v) => { since = v < peak * 0.05 ? 0 : since + stepMin / 60; return since; });
    }
    case 'rebalances': return reb.map((v) => Math.max(0, v));
  }
}
function evaluate(data, th, forMin) {
  const need = Math.max(1, Math.ceil(forMin / stepMin));
  let run = 0, episodes = 0, firingNow = false;
  data.forEach((v, i) => { run = v > th ? run + 1 : 0; if (run === need) episodes++; if (i === data.length - 1) firingNow = run >= need; });
  return { episodes, firingNow, now: data[data.length - 1] };
}
const lagA = [100,120,90,400,800,1200,1500,1900,2500,3000,3400,3900,4500,5100,5600,6000];
const lagB = [5000,4000,3000,2000,200,100,3000,6000,9000,12000,15000,18000,21000,24000,27000,30000,33000,36000,39000,42000];
const rate = lagA.map((_, i) => i % 3 === 0 ? 0 : 10 + i);
const maxPart = lagA.map((v) => v * 0.4);
const reb = lagA.map((_, i) => (i % 4) - 1);
const cases = [
  ['lag', lagA, 1000, 5], ['time_behind', lagA, 200, 3], ['partition_lag', lagA, 1500, 2],
  ['growing', lagA, 0, 3], ['growing', lagB, 0, 5], ['not_drained', lagB, 0.2, 0], ['rebalances', lagA, 1, 2],
];
const out = cases.map(([m, lag, th, forMin]) => {
  const s = series(m, lag, rate, lag === lagA ? maxPart : lag.map(v => v / 2), reb);
  const thr = m === 'growing' ? 0 : th;
  return { metric: m, series: s, ...evaluate(s, thr, forMin) };
});
console.log(JSON.stringify(out));
