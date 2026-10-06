// Development-only schema qualification against a local empty fixture.
// This is not a deployment executor; PROD receives the prebuilt reviewed SQL.
package main

import (
	"context"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"account/internal/model"
	"account/internal/overlay"
	"account/internal/tasksession"
	"github.com/jackc/pgx/v5"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func main() {
	var dsnEnv string
	flag.StringVar(&dsnEnv, "dsn-env", "NATIVE_SCHEMA_RENDER_DSN", "Environment variable for the local fixture DSN")
	flag.Parse()
	if err := render(os.Getenv(dsnEnv)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("native_schema_fixture_ready_for_schema_only_dump")
}

func render(dsn string) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Database != "accounts_native_render" || (cfg.Host != "127.0.0.1" && cfg.Host != "localhost") {
		return fmt.Errorf("renderer requires local empty accounts_native_render fixture; production targets are refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return fmt.Errorf("fixture connection failed")
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	var count int
	if err = sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname<>'information_schema' AND c.relkind IN ('r','p','v','m','S','f')`).Scan(&count); err != nil || count != 0 {
		return fmt.Errorf("renderer refuses existing application objects")
	}
	apply := func(body string) error {
		if err := db.WithContext(ctx).Exec(body).Error; err != nil {
			return fmt.Errorf("fixture SQL qualification failed")
		}
		return nil
	}
	readApply := func(name string) error {
		body, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		return apply(string(body))
	}
	if err = readApply("sql/schema.sql"); err != nil {
		return err
	}
	for _, selector := range []struct{ function, variable string }{{"applyRBACSchema", "statements"}, {"billingSchemaStatements", ""}} {
		statements, err := runtimeStatements("cmd/accountsvc/main.go", selector.function, selector.variable)
		if err != nil {
			return err
		}
		for _, statement := range statements {
			if strings.HasPrefix(strings.TrimSpace(statement), "UPDATE ") {
				continue
			}
			if err = apply(statement); err != nil {
				return err
			}
		}
	}
	if err = tasksession.ApplyPostgresSchema(ctx, sqlDB); err != nil {
		return fmt.Errorf("fixture task-session qualification failed")
	}
	if err = db.WithContext(ctx).AutoMigrate(&model.AdminSetting{}, &model.HomepageVideoSetting{}, &model.SandboxBinding{}, &model.Tenant{}, &model.TenantDomain{}, &model.TenantMembership{}, &model.XWorkmateProfile{}); err != nil {
		return fmt.Errorf("fixture model qualification failed")
	}
	if err = overlay.AutoMigrate(db.WithContext(ctx)); err != nil {
		return fmt.Errorf("fixture overlay qualification failed")
	}
	files, err := filepath.Glob("sql/migrations/*.up.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, file := range files {
		if err = readApply(file); err != nil {
			return err
		}
	}
	return apply("DROP TRIGGER IF EXISTS trg_users_maintain_email_verified ON public.users; DROP FUNCTION public.maintain_email_verified()")
}

// Select only the reviewed literal statement array, not seed statements or
// arbitrary code. AST parsing fails when a future bootstrap stops using it.
func runtimeStatements(path, function, variable string) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return nil, err
	}
	var selected *ast.CompositeLit
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != function {
			continue
		}
		for _, statement := range fn.Body.List {
			if variable == "" {
				if ret, ok := statement.(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
					selected, _ = ret.Results[0].(*ast.CompositeLit)
				}
			} else if assign, ok := statement.(*ast.AssignStmt); ok && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 {
				if name, ok := assign.Lhs[0].(*ast.Ident); ok && name.Name == variable {
					selected, _ = assign.Rhs[0].(*ast.CompositeLit)
				}
			}
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("runtime schema statement array changed; explicit review required")
	}
	var statements []string
	for _, element := range selected.Elts {
		literal, ok := element.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return nil, fmt.Errorf("runtime schema must use reviewed literal SQL")
		}
		statement, err := strconv.Unquote(literal.Value)
		if err != nil {
			return nil, err
		}
		statements = append(statements, statement)
	}
	return statements, nil
}
