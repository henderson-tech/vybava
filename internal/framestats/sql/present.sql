-- present: how the app frames reached the display (layers, gaps by vsync, CPU placement).
SELECT 'present' AS section, a.layer_name, a.present_type, a.gpu_composition, COUNT(*) AS n,
  ROUND(AVG(a.dur) / 1e6, 2) AS avg_ms
FROM actual_frame_timeline_slice a JOIN process p USING (upid)
WHERE p.name = '{{package}}'
GROUP BY a.layer_name, a.present_type, a.gpu_composition
ORDER BY n DESC;

-- display frames (SurfaceFlinger) and whether SF composited on the GPU
SELECT 'display' AS section, a.present_type, a.gpu_composition, a.jank_type, COUNT(*) AS n
FROM actual_frame_timeline_slice a JOIN process p USING (upid)
WHERE p.name LIKE '%surfaceflinger%'
GROUP BY a.present_type, a.gpu_composition, a.jank_type
ORDER BY n DESC LIMIT 12;

-- gaps between consecutive app presents (dropped vsyncs as the user sees them)
WITH f AS (
  SELECT a.ts + a.dur AS present_end
  FROM actual_frame_timeline_slice a JOIN process p USING (upid)
  WHERE p.name = '{{package}}' AND a.layer_name LIKE '%{{package}}/%'
  ORDER BY present_end
), g AS (
  SELECT present_end - LAG(present_end) OVER (ORDER BY present_end) AS gap FROM f
)
SELECT 'gaps' AS section,
  SUM(CASE WHEN gap < 1.5 * {{vsync_ns}} THEN 1 ELSE 0 END) AS one_vsync,
  SUM(CASE WHEN gap >= 1.5 * {{vsync_ns}} AND gap < 2.5 * {{vsync_ns}} THEN 1 ELSE 0 END) AS two_vsync,
  SUM(CASE WHEN gap >= 2.5 * {{vsync_ns}} AND gap < 4.5 * {{vsync_ns}} THEN 1 ELSE 0 END) AS three_to_four,
  SUM(CASE WHEN gap >= 4.5 * {{vsync_ns}} THEN 1 ELSE 0 END) AS longer
FROM g;

-- where the RenderThread and main thread ran (cpu clusters: 0-3 little, 4-5 mid, 6-7 big on Exynos 990)
WITH app AS (SELECT upid, pid FROM process WHERE name = '{{package}}'),
th AS (SELECT utid, CASE WHEN thread.tid = (SELECT pid FROM app) THEN 'main' ELSE thread.name END AS tname
       FROM thread JOIN app USING (upid) WHERE thread.name = 'RenderThread' OR thread.tid = (SELECT pid FROM app))
SELECT 'cpu' AS section, th.tname, s.cpu, ROUND(SUM(s.dur) / 1e6, 1) AS running_ms
FROM sched s JOIN th USING (utid)
GROUP BY th.tname, s.cpu ORDER BY th.tname, s.cpu;
