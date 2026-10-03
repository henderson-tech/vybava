-- frames: frame-level summary of {{package}} (timeline jank, main and RenderThread slices, SurfaceFlinger).
-- 1) frame timeline jank (app frames)
SELECT 'timeline' AS section, COUNT(*) AS frames,
  SUM(CASE WHEN jank_type != 'None' THEN 1 ELSE 0 END) AS janky,
  ROUND(AVG(dur) / 1e6, 2) AS avg_ms,
  ROUND(MAX(dur) / 1e6, 2) AS max_ms
FROM actual_frame_timeline_slice a
JOIN process p USING (upid)
WHERE p.name = '{{package}}';

SELECT 'jank_types' AS section, jank_type, COUNT(*) AS n
FROM actual_frame_timeline_slice a JOIN process p USING (upid)
WHERE p.name = '{{package}}' AND jank_type != 'None'
GROUP BY jank_type ORDER BY n DESC;

-- 2) main-thread and RenderThread slice durations (top-level names)
WITH app AS (SELECT upid FROM process WHERE name = '{{package}}'),
th AS (
  SELECT utid, thread.name AS tname FROM thread JOIN app USING (upid)
  WHERE thread.name IN ('RenderThread') OR thread.tid = (SELECT pid FROM process WHERE name = '{{package}}')
)
SELECT 'slices' AS section, th.tname, s.name, COUNT(*) AS n,
  ROUND(AVG(s.dur) / 1e6, 2) AS avg_ms,
  ROUND(MAX(s.dur) / 1e6, 2) AS max_ms,
  ROUND(SUM(s.dur) / 1e6, 1) AS total_ms
FROM slice s
JOIN thread_track tt ON s.track_id = tt.id
JOIN th USING (utid)
WHERE s.depth <= 1 AND s.name IN (
  'Choreographer#doFrame', 'traversal', 'draw', 'Record View#draw()', 'DrawFrames', 'DrawFrame',
  'syncFrameState', 'prepareTree', 'flush commands', 'dequeueBuffer', 'queueBuffer',
  'eglSwapBuffersWithDamageKHR', 'Fabric::mount', 'animation', 'input', 'measure', 'layout'
) OR (s.name LIKE 'Choreographer#doFrame%' AND s.depth = 0 AND tt.utid IN (SELECT utid FROM th))
GROUP BY th.tname, s.name ORDER BY total_ms DESC LIMIT 30;

-- 3) SurfaceFlinger composition per vsync
SELECT 'sf' AS section, s.name, COUNT(*) AS n, ROUND(AVG(s.dur) / 1e6, 2) AS avg_ms, ROUND(MAX(s.dur) / 1e6, 2) AS max_ms
FROM slice s JOIN thread_track tt ON s.track_id = tt.id JOIN thread USING (utid) JOIN process p USING (upid)
WHERE p.name LIKE '%surfaceflinger%' AND s.name IN ('composite', 'onMessageRefresh', 'commit', 'doComposition', 'REThreaded::drawLayers', 'RenderEngine::drawLayers', 'SkiaGL::drawLayers', 'Hwc2::Display::present', 'presentAndGetReleaseFences')
GROUP BY s.name ORDER BY n DESC;
