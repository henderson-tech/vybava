-- egl-anc: preset over {{package}} (perflab analyze --sql egl-anc).
WITH app AS (SELECT upid, pid FROM process WHERE name = '{{package}}'),
mt AS (SELECT utid FROM thread JOIN app USING (upid) WHERE thread.tid = (SELECT pid FROM app)),
one AS (SELECT s.id FROM slice s JOIN thread_track tt ON s.track_id = tt.id WHERE tt.utid IN (SELECT utid FROM mt) AND s.name = 'eglSwapBuffers' LIMIT 1 OFFSET 500)
SELECT a.depth, a.name, ROUND(a.dur/1e6,3) AS ms FROM one, ancestor_slice(one.id) a ORDER BY a.depth;
