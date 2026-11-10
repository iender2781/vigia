// Package store guarda la CMDB, la topología, los eventos y las ejecuciones en PostgreSQL.
//
// Todas las operaciones de negocio se hacen dentro de una transacción que fija
// app.tenant_id, de modo que las políticas RLS aíslan los datos de cada empresa.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq" // controlador de PostgreSQL
)

//go:embed migrations/*.up.sql
var migrationsFS embed.FS

// Store es el acceso a la base de datos de un tenant.
type Store struct {
	DB       *sql.DB
	TenantID string
}

// Open abre la conexión y comprueba que responde.
func Open(ctx context.Context, url, tenantID string) (*Store, error) {
	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("conectando a PostgreSQL: %w", err)
	}
	return &Store{DB: db, TenantID: tenantID}, nil
}

// Close cierra la conexión.
func (s *Store) Close() error { return s.DB.Close() }

// Tx ejecuta fn en una transacción con app.tenant_id fijado.
func (s *Store) Tx(ctx context.Context, readOnly bool, fn func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: readOnly})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, s.TenantID); err != nil {
		tx.Rollback()
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// EnsureTenant crea el tenant si no existe.
func (s *Store) EnsureTenant(ctx context.Context, name string) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO tenant (id, name) VALUES ($1, $2) ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name`,
		s.TenantID, name)
	return err
}

// Migration es una migración disponible.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

func migrations() ([]Migration, error) {
	entries, err := fs.Glob(migrationsFS, "migrations/*.up.sql")
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, e := range entries {
		base := strings.TrimPrefix(e, "migrations/")
		num, _, _ := strings.Cut(base, "_")
		v, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("migración con nombre no válido: %s", base)
		}
		b, err := migrationsFS.ReadFile(e)
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Version: v, Name: base, SQL: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Migrate aplica las migraciones pendientes, una transacción por migración y con un
// bloqueo de asesoría para que dos procesos no migren a la vez. Devuelve las aplicadas.
func Migrate(ctx context.Context, db *sql.DB) ([]string, error) {
	all, err := migrations()
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	const lockKey = 727378 // "contexto" migraciones
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		return nil, err
	}
	defer conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey) //nolint:errcheck
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version int PRIMARY KEY, name text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}
	applied := map[int]bool{}
	rows, err := conn.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil, err
		}
		applied[v] = true
	}
	rows.Close()
	var done []string
	for _, m := range all {
		if applied[m.Version] {
			continue
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return done, err
		}
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			tx.Rollback()
			return done, fmt.Errorf("migración %s: %w", m.Name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.Version, m.Name); err != nil {
			tx.Rollback()
			return done, err
		}
		if err := tx.Commit(); err != nil {
			return done, err
		}
		done = append(done, m.Name)
	}
	return done, nil
}

// SchemaVersion devuelve la última migración aplicada y la última disponible.
func SchemaVersion(ctx context.Context, db *sql.DB) (current, latest int, err error) {
	all, err := migrations()
	if err != nil {
		return 0, 0, err
	}
	if len(all) > 0 {
		latest = all[len(all)-1].Version
	}
	err = db.QueryRowContext(ctx, `SELECT COALESCE(max(version), 0) FROM schema_migrations`).Scan(&current)
	return current, latest, err
}

// MaintainPartitions crea las particiones de eventos de los próximos meses y borra las
// que superan la retención.
func (s *Store) MaintainPartitions(ctx context.Context, retentionMonths int) (created, dropped int, err error) {
	if err = s.DB.QueryRowContext(ctx, `SELECT contexto_ensure_event_partitions(3)`).Scan(&created); err != nil {
		return
	}
	if retentionMonths > 0 {
		err = s.DB.QueryRowContext(ctx, `SELECT contexto_drop_event_partitions($1)`, retentionMonths).Scan(&dropped)
	}
	return
}
