package testutil

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func randSuffix() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// pingPG verifies a DSN is usable using the pgx stdlib driver.
func pingPG(ctx context.Context, dsn string) bool {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return false
	}
	defer db.Close()
	ctx2, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return db.PingContext(ctx2) == nil
}
