-- PROPOSED ONLY: move into sql/migrations only with reviewed runtime compatibility.
-- Canonicalize only the legacy Free group label. Do not grant quota, change
-- subscription attributes, or rewrite financial / usage history.
LOCK TABLE public.users IN SHARE ROW EXCLUSIVE MODE;

UPDATE public.users AS u
SET groups = (
  SELECT COALESCE(jsonb_agg(mapped.value ORDER BY mapped.first_position), '[]'::jsonb)
  FROM (
    SELECT CASE WHEN item.value = 'segment:quota:free-3gb'
                THEN 'segment:quota:free-5gb' ELSE item.value END AS value,
           MIN(item.position) AS first_position
    FROM jsonb_array_elements_text(u.groups) WITH ORDINALITY AS item(value, position)
    GROUP BY CASE WHEN item.value = 'segment:quota:free-3gb'
                  THEN 'segment:quota:free-5gb' ELSE item.value END
  ) AS mapped
)
WHERE u.groups ? 'segment:quota:free-3gb';
