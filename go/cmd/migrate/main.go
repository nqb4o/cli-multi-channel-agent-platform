// Command migrate applies F12 Postgres migrations.
//
// Usage:
//
//	DB_DSN=postgres://... go run ./cmd/migrate up
//	DB_DSN=postgres://... go run ./cmd/migrate status
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"

	"github.com/openclaw/agent-platform/internal/persistence"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]

	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "ERROR: DB_DSN env var required")
		os.Exit(2)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: connect: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close(ctx)

	switch cmd {
	case "up":
		applied, err := persistence.Up(ctx, conn, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: up: %v\n", err)
			os.Exit(1)
		}
		if len(applied) == 0 {
			fmt.Println("No pending migrations.")
			return
		}
		fmt.Printf("Applied %d migration(s):\n", len(applied))
		for _, v := range applied {
			fmt.Printf("  + %s\n", v)
		}
	case "status":
		rows, err := persistence.Status(ctx, conn, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: status: %v\n", err)
			os.Exit(1)
		}
		for _, r := range rows {
			mark := "[ ]"
			if r.Applied {
				mark = "[x]"
			}
			fmt.Printf("  %s %s\n", mark, r.Version)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: migrate (up|status)")
}
