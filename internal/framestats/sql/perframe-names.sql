-- perframe-names: preset over {{package}} (perflab analyze --sql perframe-names).
WITH app AS (SELECT upid, pid FROM process WHERE name = '{{package}}'),
mt AS (SELECT utid FROM thread JOIN app USING (upid) WHERE thread.tid = (SELECT pid FROM app))
SELECT s.name, s.depth, COUNT(*) AS n, ROUND(AVG(s.dur)/1e6, 3) AS avg_ms
FROM slice s JOIN thread_track tt ON s.track_id = tt.id
WHERE tt.utid IN (SELECT utid FROM mt)
GROUP BY s.name, s.depth HAVING n > 1500 ORDER BY s.depth, n DESC LIMIT 40;
