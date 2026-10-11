-- Each team member's usage per bucket, under the team's current membership: the people behind a
-- team's total. A person in two teams has a row in each.
CREATE OR REPLACE VIEW mart.v_team_member_period
WITH (security_invoker = true) AS
SELECT u.tenant_id, u.bucket_start, u.bucket_size, tm.team_id, u.user_ref, u.submissions
  FROM mart.agg_user_period u
  JOIN ops.v_team_member tm ON tm.tenant_id = u.tenant_id AND tm.user_ref = u.user_ref;

GRANT SELECT ON mart.v_team_member_period TO sac_query;
