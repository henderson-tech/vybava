-- input: preset over {{package}} (perflab analyze --sql input).
WITH app AS (SELECT upid, pid FROM process WHERE name = '{{package}}'),
mt AS (SELECT utid FROM thread JOIN app USING (upid) WHERE thread.tid = (SELECT pid FROM app)),
roots AS (SELECT s.id, s.depth FROM slice s JOIN thread_track tt ON s.track_id = tt.id WHERE tt.utid IN (SELECT utid FROM mt) AND s.name LIKE 'deliverInputEvent%')
SELECT d.name, d.depth - r.depth AS rel_depth, COUNT(*) AS n, ROUND(AVG(d.dur)/1e6, 3) AS avg_ms, ROUND(SUM(d.dur)/1e6, 1) AS total_ms
FROM roots r JOIN descendant_slice(r.id) d
GROUP BY d.name, rel_depth ORDER BY total_ms DESC LIMIT 25;
