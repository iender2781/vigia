package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"vigia/internal/contexto/engine"
	"vigia/internal/contexto/ontology"
	"vigia/internal/contexto/prom"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func cmdOntology(args []string) error {
	if len(args) == 0 {
		return errors.New("uso: contexto ontology validate|summary|import-legacy [opciones]")
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("ontology "+sub, flag.ExitOnError)
	dir := fs.String("dir", envOr("CONTEXTO_ONTOLOGY_DIR", "ontology"), "directorio de la ontología")
	switch sub {
	case "validate", "summary":
		_ = fs.Parse(args)
		o, err := ontology.Load(*dir)
		if err != nil {
			return err
		}
		if _, err := engine.New(o); err != nil {
			return err
		}
		fmt.Printf("Ontología válida: %s\n", *dir)
		fmt.Printf("  %d clases en %d tecnologías, %d tipos de relación, %d reglas en %d packs\n",
			len(o.Classes), len(o.Technologies()), len(o.Relations), len(o.Rules), len(o.Packs))
		if sub == "summary" {
			for _, p := range o.Packs {
				fmt.Printf("  pack %-12s %-15s %3d reglas\n", p.Name, p.Technology, len(p.Rules))
			}
			fmt.Printf("  relaciones manuales: %d ficheros, %d entradas\n", len(o.Manual.Files), len(o.Manual.Entries))
		}
		return nil
	case "import-legacy":
		cfg := fs.String("config", "", "config_junta.json del correlador Python")
		out := fs.String("out", "", "directorio de salida (no debe existir, salvo con -force)")
		force := fs.Bool("force", false, "sobrescribir el directorio de salida")
		_ = fs.Parse(args)
		if *cfg == "" || *out == "" {
			return errors.New("indica -config y -out")
		}
		if _, err := os.Stat(*out); err == nil && !*force {
			return fmt.Errorf("%s ya existe (usa -force para sobrescribir)", *out)
		}
		o, rep, err := ontology.ImportLegacy(*cfg)
		if err != nil {
			return err
		}
		if err := o.WriteDir(*out); err != nil {
			return err
		}
		if _, err := ontology.Load(*out); err != nil {
			return fmt.Errorf("la ontología importada no valida: %w", err)
		}
		fmt.Printf("Importadas %d reglas y %d clases en %s\n", rep.Rules, rep.Classes, *out)
		for p, n := range rep.Packs {
			fmt.Printf("  %-12s %d reglas\n", p, n)
		}
		for _, w := range rep.Warnings {
			fmt.Println("  AVISO:", w)
		}
		return nil
	}
	return fmt.Errorf("subcomando desconocido: ontology %s", sub)
}

func cmdReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	dir := fs.String("ontology", envOr("CONTEXTO_ONTOLOGY_DIR", "ontology"), "directorio de la ontología")
	seriesFile := fs.String("series-file", "", "captura de /api/v1/series (series.json)")
	valuesFile := fs.String("values-file", "", "captura de valores (values.json o respuesta de /api/v1/query)")
	alertsFile := fs.String("alerts-file", "", "captura de alertas (Alertmanager v2 o Prometheus)")
	export := fs.String("export", "", "directorio donde escribir cmdb_ci.csv y cmdb_relaciones.csv")
	jsonOut := fs.String("json", "", "fichero JSON con la instantánea completa")
	noExclude := fs.Bool("no-exclude", false, "no aplicar exclude_jobs (para comparar con --cmdb-export de Python)")
	_ = fs.Parse(args)
	if *seriesFile == "" {
		return errors.New("indica -series-file")
	}
	o, err := ontology.Load(*dir)
	if err != nil {
		return err
	}
	eng, err := engine.New(o)
	if err != nil {
		return err
	}
	t0 := time.Now()
	in, err := readCapture(*seriesFile, *valuesFile, *alertsFile)
	if err != nil {
		return err
	}
	if !*noExclude {
		in = excludeJobs(in, o.Settings.ExcludeJobs)
	}
	tRead := time.Since(t0)
	res := eng.Build(in)
	tBuild := time.Since(t0) - tRead
	fmt.Fprintf(os.Stderr, "CMDB: %d elementos, %d relaciones (lectura %.1f s, construcción %.2f s)\n",
		len(res.CIs), len(res.Rels), tRead.Seconds(), tBuild.Seconds())
	var parts []string
	for _, cc := range res.Summary() {
		parts = append(parts, cc.String())
	}
	fmt.Fprintln(os.Stderr, "  "+strings.Join(parts, ", "))
	for _, m := range res.Stats.ManualErrors {
		fmt.Fprintln(os.Stderr, "  AVISO:", m)
	}
	if *export != "" {
		if err := res.WriteCSV(*export); err != nil {
			return err
		}
	}
	if *jsonOut != "" {
		return res.WriteJSON(*jsonOut)
	}
	return nil
}

func readCapture(seriesFile, valuesFile, alertsFile string) (engine.Input, error) {
	var in engine.Input
	b, err := os.ReadFile(seriesFile)
	if err != nil {
		return in, err
	}
	if in.Series, err = prom.ParseSeries(b); err != nil {
		return in, fmt.Errorf("%s: %w", seriesFile, err)
	}
	if valuesFile != "" {
		b, err := os.ReadFile(valuesFile)
		if err != nil {
			return in, err
		}
		if in.Values, err = prom.ParseValues(b); err != nil {
			return in, fmt.Errorf("%s: %w", valuesFile, err)
		}
	}
	if alertsFile != "" {
		b, err := os.ReadFile(alertsFile)
		if err != nil {
			return in, err
		}
		alerts, err := prom.ParseAlerts(b)
		if err != nil {
			return in, fmt.Errorf("%s: %w", alertsFile, err)
		}
		for _, a := range alerts {
			in.Alerts = append(in.Alerts, a.Labels)
		}
	}
	return in, nil
}

func excludeJobs(in engine.Input, jobs []string) engine.Input {
	if len(jobs) == 0 {
		return in
	}
	ex := map[string]bool{}
	for _, j := range jobs {
		ex[j] = true
	}
	out := engine.Input{}
	for _, s := range in.Series {
		if !ex[s["job"]] {
			out.Series = append(out.Series, s)
		}
	}
	for _, v := range in.Values {
		if !ex[v.Labels["job"]] {
			out.Values = append(out.Values, v)
		}
	}
	for _, a := range in.Alerts {
		if !ex[a["job"]] {
			out.Alerts = append(out.Alerts, a)
		}
	}
	return out
}
