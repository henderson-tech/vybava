-- jank: preset over {{package}} (perflab analyze --sql jank).
SELECT a.jank_type, a.present_type, COUNT(*) AS n, ROUND(AVG(a.dur)/1e6,2) AS avg_ms
FROM actual_frame_timeline_slice a JOIN process p USING (upid)
WHERE p.name = '{{package}}' GROUP BY a.jank_type, a.present_type ORDER BY n DESC LIMIT 8;
