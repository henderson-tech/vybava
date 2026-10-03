-- layer: preset over {{package}} (perflab analyze --sql layer).
WITH app AS (SELECT upid FROM process WHERE name = '{{package}}'),
rt AS (SELECT utid FROM thread JOIN app USING (upid) WHERE thread.name = 'RenderThread')
SELECT s.name, COUNT(*) AS n, ROUND(AVG(s.dur)/1e6, 2) AS avg_ms FROM slice s JOIN thread_track tt ON s.track_id = tt.id
WHERE tt.utid IN (SELECT utid FROM rt) AND (s.name LIKE 'drawLayer%' OR s.name LIKE 'Drawing %')
GROUP BY s.name;
