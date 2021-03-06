package main

import (
	"database/sql"
	"log"
)

type migration struct {
	ID string
	Up string
}

// mustMigrate panics if it fails.
func mustMigrate(db *sql.DB) {
	migrations := []migration{
		{
			ID: "1",
			Up: `
				CREATE TABLE cache (
					id STRING PRIMARY KEY,
					until TIMESTAMP,
					data BYTES,
					gzip BYTES,
					last_hit TIMESTAMP
				);

				CREATE TABLE config (
					key STRING PRIMARY KEY,
					i INT NULL,
					s STRING NULL
				);
			`,
		},
		{
			ID: "2",
			Up: `
				CREATE TABLE IF NOT EXISTS playerskills (
					region INT,
					blizzid INT,
					build INT,
					mode INT,
					skill FLOAT,
					PRIMARY KEY (region, blizzid, build, mode),
					INDEX (region ASC, build ASC, mode ASC, skill DESC) STORING (blizzid)
				);

				CREATE TABLE IF NOT EXISTS skillstats (
					build INT,
					mode INT,
					data JSONB,
					PRIMARY KEY (build, mode)
				);
			`,
		},
		{
			ID: "3",
			Up: `
				CREATE TABLE IF NOT EXISTS leaderboard (
					region INT NOT NULL,
					mode INT NOT NULL,
					rank INT NOT NULL,
					blizzid INT NULL,
					skill FLOAT NULL,
					total INT NULL,
					recent INT NULL,
					CONSTRAINT "primary" PRIMARY KEY (region ASC, mode ASC, rank ASC)
				);
			`,
		},
	}

	const migrateTable = "migrations"

	mustExec(db, `CREATE TABLE IF NOT EXISTS `+migrateTable+` (
		id string PRIMARY KEY,
		created timestamp DEFAULT NOW()
	)`)

	var n int
	seen := make(map[string]bool)
	for _, migration := range migrations {
		// Sanity checks.
		if migration.ID == "" {
			panic("empty migration ID")
		}
		if seen[migration.ID] {
			panic("duplicate ID")
		}
		seen[migration.ID] = true

		// Check if migration has been run already.
		var i int
		if err := db.QueryRow("SELECT count(*) from "+migrateTable+" WHERE id = $1", migration.ID).Scan(&i); err != nil {
			panic(err)
		}
		if i != 0 {
			continue
		}

		// Migrate.
		mustExec(db, migration.Up)
		n++

		mustExec(db, "INSERT INTO "+migrateTable+" (id) VALUES ($1)", migration.ID)
	}
	// Clear the cache because implementations may have changed. This assumes
	// the cron job is running the correct image, which may not be true.
	mustExec(db, `UPDATE cache SET until = NULL`)
	log.Printf("applied %d migrations", n)
}
