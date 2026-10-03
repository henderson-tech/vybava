-- perframe: one row per app frame: vsync id, present end, main doFrame ms, RT DrawFrames ms.
WITH app AS (SELECT upid, pid FROM process WHERE name = '{{package}}'),
mt AS (SELECT utid FROM thread JOIN app USING (upid) WHERE thread.tid = (SELECT pid FROM app)),
rt AS (SELECT utid FROM thread JOIN app USING (upid) WHERE thread.name = 'RenderThread'),
f AS (
  SELECT CAST(a.name AS INT) AS vsync, a.ts, a.ts + a.dur AS present_end
  FROM actual_frame_timeline_slice a JOIN app USING (upid)
  WHERE a.layer_name LIKE '%{{package}}/%'
),
d AS (
  SELECT CAST(SUBSTR(s.name, 23) AS INT) AS vsync, s.ts AS do_ts, s.dur AS do_dur
  FROM slice s JOIN thread_track tt ON s.track_id = tt.id
  WHERE tt.utid IN (SELECT utid FROM mt) AND s.name LIKE 'Choreographer#doFrame %'
),
r AS (
  SELECT CAST(SUBSTR(s.name, 12) AS INT) AS vsync, s.dur AS rt_dur
  FROM slice s JOIN thread_track tt ON s.track_id = tt.id
  WHERE tt.utid IN (SELECT utid FROM rt) AND s.name LIKE 'DrawFrames %'
),
inp AS (
  SELECT s.ts FROM slice s JOIN thread_track tt ON s.track_id = tt.id
  WHERE tt.utid IN (SELECT utid FROM mt) AND s.name LIKE 'deliverInputEvent%'
)
SELECT f.vsync, f.present_end,
  ROUND(d.do_dur / 1e6, 2) AS main_ms, ROUND(r.rt_dur / 1e6, 2) AS rt_ms,
  (SELECT COUNT(*) FROM inp WHERE inp.ts BETWEEN f.ts - 1500e6 AND f.ts) AS recent_input
FROM f LEFT JOIN d USING (vsync) LEFT JOIN r USING (vsync)
ORDER BY f.present_end;
