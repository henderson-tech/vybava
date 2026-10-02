-- anim-kids: preset over {{package}} (perflab analyze --sql anim-kids).
WITH app AS (SELECT upid, pid FROM process WHERE name = '{{package}}'),
mt AS (SELECT utid FROM thread JOIN app USING (upid) WHERE thread.tid = (SELECT pid FROM app)),
one AS (SELECT s.id, s.ts, s.dur FROM slice s JOIN thread_track tt ON s.track_id = tt.id WHERE tt.utid IN (SELECT utid FROM mt) AND s.name = 'animation' LIMIT 1 OFFSET 500)
SELECT d.depth, d.name, ROUND((d.ts - one.ts)/1e6,3) AS at_ms, ROUND(d.dur/1e6,3) AS ms FROM one, descendant_slice(one.id) d ORDER BY d.ts LIMIT 30;
