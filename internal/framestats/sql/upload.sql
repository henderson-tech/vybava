-- upload: preset over {{package}} (perflab analyze --sql upload).
WITH app AS (SELECT upid, pid FROM process WHERE name = '{{package}}'),
th AS (SELECT utid, CASE WHEN thread.tid = (SELECT pid FROM app) THEN 'main' ELSE thread.name END AS tname FROM thread JOIN app USING (upid))
SELECT th.tname, s.name, COUNT(*) AS n, ROUND(AVG(s.dur)/1e6, 3) AS avg_ms, ROUND(MAX(s.dur)/1e6, 2) AS max_ms, ROUND(SUM(s.dur)/1e6, 1) AS total_ms
FROM slice s JOIN thread_track tt ON s.track_id = tt.id JOIN th USING (utid)
WHERE s.name LIKE '%pload%' OR s.name LIKE '%Bitmap%' OR s.name LIKE 'deliverInputEvent%' OR s.name LIKE '%MountItem%' OR s.name LIKE 'Fabric%' OR s.name LIKE '%commit%' OR s.name LIKE 'RV %'
GROUP BY th.tname, CASE WHEN s.name LIKE 'deliverInputEvent%' THEN 'deliverInputEvent' ELSE s.name END
ORDER BY total_ms DESC LIMIT 20;
