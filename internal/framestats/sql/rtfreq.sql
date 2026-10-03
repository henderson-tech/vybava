-- rtfreq: preset over {{package}} (perflab analyze --sql rtfreq).
WITH app AS (SELECT upid FROM process WHERE name = '{{package}}'),
rt AS (SELECT utid FROM thread JOIN app USING (upid) WHERE thread.name = 'RenderThread'),
draws AS (
  SELECT s.ts, s.dur FROM slice s JOIN thread_track tt ON s.track_id = tt.id
  WHERE tt.utid IN (SELECT utid FROM rt) AND s.name LIKE 'Drawing %'
),
placed AS (
  SELECT d.ts, d.dur, (SELECT sc.cpu FROM sched sc WHERE sc.utid IN (SELECT utid FROM rt) AND sc.ts <= d.ts AND sc.ts + sc.dur > d.ts LIMIT 1) AS cpu
  FROM draws d
),
freq AS (
  SELECT p.ts, p.dur, p.cpu,
    (SELECT c.value FROM counter c JOIN cpu_counter_track t ON c.track_id = t.id
     WHERE t.name = 'cpufreq' AND t.cpu = p.cpu AND c.ts <= p.ts ORDER BY c.ts DESC LIMIT 1) AS khz
  FROM placed p
)
SELECT cpu, CAST(khz / 1000 AS INT) AS mhz, COUNT(*) AS n, ROUND(AVG(dur)/1e6, 2) AS avg_draw_ms
FROM freq GROUP BY cpu, mhz ORDER BY n DESC LIMIT 15;
