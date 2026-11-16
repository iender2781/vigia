package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vigia/internal/contexto/engine"
	"vigia/internal/contexto/ontology"
	"vigia/internal/contexto/prom"
	"vigia/internal/contexto/store"
)

// Test de integración: necesita CONTEXTO_TEST_DATABASE_URL apuntando a una base de datos
// vacía y desechable (el CI levanta un PostgreSQL de servicio). Sin ella, se omite.

const tenant = "11111111-2222-3333-4444-555555555555"

func setup(t *testing.T) (*store.Store, *engine.Engine, engine.Input) {
	t.Helper()
	url := os.Getenv("CONTEXTO_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CONTEXTO_TEST_DATABASE_URL no definida")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url, tenant)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, q := range []string{`DROP SCHEMA public CASCADE`, `CREATE SCHEMA public`} {
		if _, err := st.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Migrate(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	if done, err := store.Migrate(ctx, st.DB); err != nil || len(done) != 0 {
		t.Fatalf("la segunda migración debería no hacer nada: %v %v", done, err)
	}
	if err := st.EnsureTenant(ctx, "pruebas"); err != nil {
		t.Fatal(err)
	}
	o, err := ontology.Load("../../../ontology")
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(o)
	if err != nil {
		t.Fatal(err)
	}
	fx := filepath.Join("..", "engine", "testdata", "fixture")
	b, _ := os.ReadFile(filepath.Join(fx, "series.json"))
	series, err := prom.ParseSeries(b)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(fx, "values.json"))
	values, _ := prom.ParseValues(b)
	return st, eng, engine.Input{Series: series, Values: values}
}

func count(t *testing.T, st *store.Store, q string, args ...any) int {
	t.Helper()
	n := 0
	err := st.Tx(context.Background(), true, func(tx *sql.Tx) error {
		return tx.QueryRow(q, args...).Scan(&n)
	})
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestApplySnapshotLifecycle(t *testing.T) {
	st, eng, in := setup(t)
	ctx := context.Background()
	opts := store.ApplyOptions{StaleAfter: time.Minute, RetireAfter: time.Hour, SSIIAttr: "COD_SSII"}
	t0 := time.Now()

	// 1. Carga inicial
	opts.Now = t0
	res := eng.Build(in)
	ap, err := st.ApplySnapshot(ctx, res, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !ap.InitialLoad || ap.Created != len(res.CIs) || ap.RelAdded != len(res.Rels) {
		t.Fatalf("carga inicial inesperada: %+v", ap)
	}

	// 2. Misma instantánea: no hay cambios
	opts.Now = t0.Add(30 * time.Second)
	ap, err = st.ApplySnapshot(ctx, eng.Build(in), opts)
	if err != nil {
		t.Fatal(err)
	}
	if ap.Created+ap.Changed+ap.RelAdded+ap.RelRemoved+len(ap.Lifecycle) != 0 {
		t.Fatalf("la segunda aplicación debería ser idempotente: %+v", ap)
	}

	// 3. Desaparece un job sano y otro está caído
	var reduced engine.Input
	reduced.Values = in.Values
	for _, s := range in.Series {
		if s["job"] == "job-veeam" || s["job"] == "job-panos" {
			continue
		}
		reduced.Series = append(reduced.Series, s)
	}
	opts.Now = t0.Add(2 * time.Minute)
	opts.JobsDown = map[string]bool{"job-panos": true}
	ap, err = st.ApplySnapshot(ctx, eng.Build(reduced), opts)
	if err != nil {
		t.Fatal(err)
	}
	if ap.Lifecycle["stale"] == 0 || ap.Lifecycle["nodata"] == 0 {
		t.Fatalf("se esperaban CI obsoletos y sin datos: %+v", ap.Lifecycle)
	}
	if n := count(t, st, `SELECT count(*) FROM ci WHERE lifecycle = 'stale' AND 'job-veeam' = ANY(jobs)`); n == 0 {
		t.Error("los CI de job-veeam deberían estar obsoletos")
	}
	if n := count(t, st, `SELECT count(*) FROM ci WHERE lifecycle = 'stale' AND 'job-panos' = ANY(jobs)`); n != 0 {
		t.Error("los CI de un job caído no deben pasar a obsoletos")
	}

	// 4. Pasado el plazo de retirada, los obsoletos se retiran y sus relaciones se cierran
	opts.Now = t0.Add(2 * time.Hour)
	opts.JobsDown = nil
	ap, err = st.ApplySnapshot(ctx, eng.Build(reduced), opts)
	if err != nil {
		t.Fatal(err)
	}
	if ap.Lifecycle["retired"] == 0 {
		t.Fatalf("se esperaban CI retirados: %+v", ap.Lifecycle)
	}
	if n := count(t, st, `SELECT count(*) FROM relation r JOIN ci c ON c.id IN (r.src, r.dst)
		WHERE r.valid_to IS NULL AND c.lifecycle = 'retired'`); n != 0 {
		t.Errorf("quedan %d relaciones vigentes con CI retirados", n)
	}

	// 5. Vuelve todo: los CI se recuperan
	opts.Now = t0.Add(3 * time.Hour)
	ap, err = st.ApplySnapshot(ctx, eng.Build(in), opts)
	if err != nil {
		t.Fatal(err)
	}
	if ap.Revived == 0 {
		t.Fatalf("se esperaban CI recuperados: %+v", ap)
	}
	if n := count(t, st, `SELECT count(*) FROM ci WHERE lifecycle <> 'active'`); n != 0 {
		t.Errorf("quedan %d CI no activos", n)
	}
	if n := count(t, st, `SELECT count(*) FROM event WHERE type = 'ci.lifecycle'`); n == 0 {
		t.Error("no se registraron eventos de ciclo de vida")
	}

	// 6. Aislamiento por tenant (RLS): otro tenant no ve nada
	other, err := store.Open(ctx, os.Getenv("CONTEXTO_TEST_DATABASE_URL"), "99999999-9999-9999-9999-999999999999")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var super bool
	_ = other.DB.QueryRow(`SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super)
	if super {
		t.Log("el usuario de pruebas es superusuario: RLS no se aplica, se omite la comprobación de aislamiento")
	} else if n := count(t, other, `SELECT count(*) FROM ci`); n != 0 {
		t.Errorf("RLS: otro tenant ve %d CI", n)
	}

	// 7. Búsqueda
	items, total, err := st.Search(ctx, store.SearchFilter{Q: "ccobuz38", SearchAttrs: []string{"COD_SSII"}})
	if err != nil || total == 0 || len(items) == 0 {
		t.Fatalf("búsqueda: %d %v", total, err)
	}
	fmt.Println("búsqueda ccobuz38:", total, "resultados")
}
