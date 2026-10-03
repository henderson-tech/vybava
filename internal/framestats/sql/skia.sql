-- skia: preset over {{package}} (perflab analyze --sql skia).
WITH app AS (SELECT upid FROM process WHERE name = '{{package}}'),
rt AS (SELECT utid FROM thread JOIN app USING (upid) WHERE thread.name = 'RenderThread')
SELECT s.name, COUNT(*) AS n, ROUND(AVG(s.dur)/1e3, 1) AS avg_us, ROUND(SUM(s.dur)/1e6, 1) AS total_ms
FROM slice s JOIN thread_track tt ON s.track_id = tt.id
WHERE tt.utid IN (SELECT utid FROM rt) AND s.depth >= 2
GROUP BY s.name ORDER BY total_ms DESC LIMIT 30;
