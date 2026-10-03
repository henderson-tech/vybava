-- children: what runs inside the main thread's per-frame phases and the RenderThread frame.
WITH app AS (SELECT upid, pid FROM process WHERE name = '{{package}}'),
mt AS (SELECT utid FROM thread JOIN app USING (upid) WHERE thread.tid = (SELECT pid FROM app)),
rt AS (SELECT utid FROM thread JOIN app USING (upid) WHERE thread.name = 'RenderThread'),
parents AS (
  SELECT s.id, s.name FROM slice s JOIN thread_track tt ON s.track_id = tt.id
  WHERE tt.utid IN (SELECT utid FROM mt) AND s.name IN ('animation', 'traversal', 'input')
)
SELECT 'main-children' AS section, p.name AS phase, c.name, COUNT(*) AS n,
  ROUND(AVG(c.dur) / 1e6, 3) AS avg_ms, ROUND(SUM(c.dur) / 1e6, 1) AS total_ms
FROM slice c JOIN parents p ON c.parent_id = p.id
GROUP BY p.name, c.name ORDER BY total_ms DESC LIMIT 25;

WITH app AS (SELECT upid FROM process WHERE name = '{{package}}'),
rt AS (SELECT utid FROM thread JOIN app USING (upid) WHERE thread.name = 'RenderThread')
SELECT 'rt' AS section, s.name, s.depth, COUNT(*) AS n, ROUND(AVG(s.dur) / 1e6, 3) AS avg_ms, ROUND(SUM(s.dur) / 1e6, 1) AS total_ms
FROM slice s JOIN thread_track tt ON s.track_id = tt.id
WHERE tt.utid IN (SELECT utid FROM rt) AND s.depth <= 2
GROUP BY s.name, s.depth ORDER BY total_ms DESC LIMIT 20;

-- other app threads that are busy (JS, UI runtime, mqt_*)
WITH app AS (SELECT upid FROM process WHERE name = '{{package}}')
SELECT 'threads' AS section, thread.name, ROUND(SUM(s.dur) / 1e6, 1) AS running_ms
FROM sched s JOIN thread USING (utid) JOIN app USING (upid)
GROUP BY thread.name ORDER BY running_ms DESC LIMIT 12;
