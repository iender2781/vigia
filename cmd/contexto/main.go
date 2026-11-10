// Command contexto es el binario único de la plataforma de contexto de Vigía
// (CMDB, topología y RCA sobre Prometheus). Cada subcomando o rol se elige al arrancar.
//
//	contexto serve --role api|discovery|all     servicio (Kubernetes)
//	contexto migrate                            aplica las migraciones de base de datos
//	contexto replay                             construye la CMDB desde una captura guardada
//	contexto ontology validate|import-legacy    gestión de la ontología
//	contexto version
package main

import (
	"fmt"
	"os"
)

// Se rellenan al compilar con -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "none"
)

func usage() {
	fmt.Fprint(os.Stderr, `Uso: contexto <comando> [opciones]

Comandos:
  serve      Arranca el servicio con el rol indicado (--role api|discovery|all)
  migrate    Aplica las migraciones pendientes de la base de datos
  replay     Construye la CMDB a partir de una captura (series/valores/alertas)
  ontology   validate | import-legacy | summary
  version    Muestra la versión

Ejecuta "contexto <comando> -h" para ver las opciones de cada uno.
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "migrate":
		err = cmdMigrate(args)
	case "replay":
		err = cmdReplay(args)
	case "ontology":
		err = cmdOntology(args)
	case "version", "--version", "-v":
		fmt.Printf("contexto %s (%s)\n", version, commit)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "comando desconocido: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
