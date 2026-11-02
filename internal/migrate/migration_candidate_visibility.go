package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// migrationCandidateVisibility accepts only the explicit migration identity and
// a complete, unrestricted role-specific policy. Existing privileged/owner
// checks remain separate; this does not grant permissions or change policies.
func migrationCandidateVisibility(ctx context.Context, tx *sql.Tx, name string) error {
	var valid bool
	err := tx.QueryRowContext(ctx, `SELECT current_user = 'accounts_migration'
 AND EXISTS (SELECT 1 FROM pg_roles r WHERE r.rolname=current_user AND r.rolcanlogin AND NOT r.rolsuper AND NOT r.rolcreatedb AND NOT r.rolcreaterole AND NOT r.rolreplication AND NOT r.rolbypassrls AND NOT r.rolinherit)
 AND NOT EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.member=(SELECT oid FROM pg_roles WHERE rolname=current_user))
 AND has_table_privilege(current_user,c.oid,'SELECT')
 AND EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid=c.oid AND p.polname='migration_candidate_access' AND p.polcmd='*' AND p.polpermissive
 AND p.polroles=ARRAY[(SELECT oid FROM pg_roles WHERE rolname=current_user)] AND pg_get_expr(p.polqual,p.polrelid)='true' AND pg_get_expr(p.polwithcheck,p.polrelid)='true')
 AND NOT EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid=c.oid AND NOT p.polpermissive AND p.polcmd IN ('r','*') AND (0::oid=ANY(p.polroles) OR (SELECT oid FROM pg_roles WHERE rolname=current_user)=ANY(p.polroles)))
 FROM pg_class c WHERE c.relnamespace='public'::regnamespace AND c.relname=$1 AND c.relkind IN ('r','p')`, name).Scan(&valid)
	if err != nil || !valid {
		return fmt.Errorf("complete target visibility is not proven for public.%s", name)
	}
	return nil
}
