package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/freetype"
	"github.com/golang/freetype/truetype"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"github.com/pkg/errors"
	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/sync/errgroup"
)

var (
	flagInit      = flag.Bool("init", false, "drop database before starting")
	flagAddr      = flag.String("addr", ":4001", "address to serve; HTTP redirect address if -autocert is set")
	flagAutocert  = flag.String("autocert", "", "domain name to autocert")
	flagAcmedir   = flag.String("acmedir", "", "optional acme directory; can be used to configure dev letsencrypt")
	flagCockroach = flag.String("cockroach", "postgresql://root@localhost:26257/hots?sslmode=disable", "cockroach connection URL")
	flagElo       = flag.Bool("elo", false, "run elo update")
	flagMigrate   = flag.Bool("migrate", false, "run migration")
	flagCron      = flag.Bool("cron", false, "run cronjob")
	flagUpdateNew = flag.String("updatenew", "", "run new update to specified gs bucket")
	flagImport    = flag.String("import", "csv.hots.dog", "import from bucket")
	flagImportNum = flag.Int("importnum", -1, "max id to import; set to 0 for first block only")
	flagUpdateDB  = flag.Bool("updatedb", false, "update db from import bucket")
	initDB        = false

	popularGameLimit = 10
)

func main() {
	flag.Parse()

	if fromEnv := os.Getenv("ADDR"); fromEnv != "" {
		*flagAddr = fromEnv
	}
	if fromEnv := os.Getenv("COCKROACH"); fromEnv != "" {
		*flagCockroach = fromEnv
	}
	if fromEnv := os.Getenv("AUTOCERT"); fromEnv != "" {
		*flagAutocert = fromEnv
	}
	if fromEnv := os.Getenv("ACMEDIR"); fromEnv != "" {
		*flagAcmedir = fromEnv
	}

	if *flagUpdateNew != "" {
		if err := updateNew(*flagUpdateNew); err != nil {
			log.Fatalf("%+v", err)
		}
		return
	}

	const dbName = "hots"
	dbURL, err := url.Parse(*flagCockroach)
	if err != nil {
		log.Fatal(err)
	}
	dbURL.Path = dbName

	if *flagElo {
		if err := elo(dbURL.String()); err != nil {
			log.Fatalf("%+v", err)
		}
		return
	}

	db := mustInitDB(dbURL.String())
	defer db.Close()

	h := &hotsContext{
		db: db,
		x:  sqlx.NewDb(db, "postgres"),
	}

	if *flagImportNum != -1 {
		mustMigrate(db)
		if err := h.Import(*flagImport, *flagImportNum); err != nil {
			log.Fatalf("%+v", err)
		}
		return
	}

	/*
	   The database cache has two timestamps: until and last_hit. last_hit is
	   the last time a user request hit the URL. until is the time after which
	   the request should be re-processed. A cron job routinely clears out
	   old cache entries whose last_hit field is older than some threshold
	   (2 days or so?). The same cron job also re-processes entries whose
	   until time has passed, and resets the until time for some small
	   threshold in the future (1 hour)?. Thus, the user code should never
	   consult any timestamps in the cache table.

	   When the in-memory cache is used, the table's last_hit column is not
	   updated. That field is only updated when the cache table is consulted
	   for a hit. This means that writes don't have to happen in most user
	   requests, and at worst the last_hit field will be out-of-date for
	   whatever the until length (1 hour) is.
	*/
	h.cacheTime = time.Hour
	if *flagInit {
		h.cacheTime = time.Second * 5
		popularGameLimit = 2
	}

	if *flagCron {
		if err := h.cronLoop(); err != nil {
			log.Fatalf("%+v", err)
		}
		return
	}
	if *flagUpdateDB {
		if err := h.updateDB(); err != nil {
			log.Fatalf("%+v", err)
		}
		return
	}

	h.mu.cache = make(map[string]cache)

	if *flagInit {
		// Don't exit on panic; prevents modd from spinning.
		defer func() {
			return
			if r := recover(); r != nil {
				fmt.Printf("%+v\n", r)
				select {}
			}
		}()
		if initDB {
			time.Sleep(time.Second * 2)
			mustExec(db, fmt.Sprintf("drop database if exists %s cascade; create database %[1]s", dbName))
		}
	}

	if *flagMigrate || *flagInit {
		mustMigrate(db)
		if initDB {
			if err := h.Import(*flagImport, *flagImportNum); err != nil {
				log.Fatalf("%+v", err)
			}
		}
		if err := h.syncConfig(*flagImport); err != nil {
			log.Fatalf("%+v", err)
		}
		if !*flagInit {
			return
		}
		h.ClearCache(nil, nil)
	}

	if err := h.updateInit(context.Background()); err != nil {
		panic(fmt.Sprintf("%+v", err))
	}

	enableCache := !*flagInit

	wrap := func(f func(context.Context, *http.Request) (interface{}, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second*60)
			defer cancel()
			if v, err := url.ParseQuery(r.URL.RawQuery); err == nil {
				r.URL.RawQuery = v.Encode()
			}
			url := r.URL.String()
			start := time.Now()
			defer func() { fmt.Printf("%s: %s\n", url, time.Since(start)) }()
			if enableCache && h.CheckCache(ctx, start, w, r, r.URL.Path, url) {
				return
			}
			res, err := f(ctx, r)
			if err != nil {
				log.Printf("%s: %+v", url, err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			data, gzip, err := resultToBytes(res)
			if err != nil {
				log.Printf("%s: %v", url, err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeDataGzip(w, r, data, gzip)
			if enableCache {
				go h.WriteCache(r.URL.Path, url, start, data, gzip)
			}
		}
	}

	http.Handle("/api/init", wrap(h.Init))
	http.Handle("/api/get-build-winrates", wrap(h.GetBuildWinrates))
	http.Handle("/api/get-compare-hero", wrap(h.GetCompareHero))
	http.Handle("/api/get-game-data", wrap(h.GetGameData))
	http.Handle("/api/get-hero-data", wrap(h.GetHero))
	http.Handle("/api/get-player-by-name", wrap(h.GetPlayerName))
	http.Handle("/api/get-player-data", wrap(h.GetPlayerData))
	http.Handle("/api/get-winrates", wrap(h.GetWinrates))
	if *flagInit {
		http.HandleFunc("/api/clear-cache", h.ClearCache)
	}

	fileServer := http.FileServer(http.Dir("static"))
	serveFiles := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/service-worker.js" {
			w.Header().Add("Cache-Control", "no-cache")
		} else if strings.HasPrefix(r.URL.Path, "/static") {
			// These have unique names and shouldn't ever change.
			w.Header().Add("Cache-Control", "max-age=31536000")
		} else {
			w.Header().Add("Cache-Control", "max-age=3600")
		}
		fileServer.ServeHTTP(w, r)
	}
	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/"
		serveFiles(w, r)
	}

	talents := make(map[string]bool)
	if err := filepath.Walk(filepath.Join("static", "img", "talent"), func(path string, info os.FileInfo, err error) error {
		talents[info.Name()] = true
		return nil
	}); err != nil {
		log.Fatal(err)
	}
	http.HandleFunc("/img/talent/", func(w http.ResponseWriter, r *http.Request) {
		base := filepath.Base(r.URL.Path)
		if !talents[base] {
			makeTalentImg(w, r)
			return
		}
		serveFiles(w, r)
	})

	http.HandleFunc("/about/", serveIndex)
	http.HandleFunc("/compare/", serveIndex)
	http.HandleFunc("/games/", serveIndex)
	http.HandleFunc("/heroes/", serveIndex)
	http.HandleFunc("/players/", serveIndex)
	http.HandleFunc("/talents/", serveIndex)
	http.HandleFunc("/", serveFiles)

	go func() {
		for range time.Tick(time.Minute * 10) {
			if err := h.updateInit(context.Background()); err != nil {
				panic(fmt.Sprintf("%+v", err))
			}
			// Clear the memory cache of old entries.
			h.mu.Lock()
			cutoff := time.Now().Add(-time.Hour).Unix()
			for s, c := range h.mu.cache {
				if c.until < cutoff {
					delete(h.mu.cache, s)
				}
			}
			h.mu.Unlock()
		}
	}()

	if *flagAutocert != "" {
		go func() {
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				u := r.URL
				u.Scheme = "https"
				u.Host = *flagAutocert
				// 301
				http.Redirect(w, r, u.String(), http.StatusMovedPermanently)
			})
			log.Fatal(http.ListenAndServe(*flagAddr, mux))
		}()
		fmt.Println("AUTOCERT on:", *flagAutocert)
		if *flagAcmedir != "" {
			fmt.Println("ACMEDIR:", *flagAcmedir)
		}
		const cloudflareOrigin = "cloudflare-origin"
		var tlsConfig *tls.Config
		if *flagAcmedir == cloudflareOrigin {
			var certfile, keyfile []byte
			if err := h.x.Get(&certfile, `SELECT s FROM config WHERE key = $1`, cloudflareOrigin+"-cert"); err != nil {
				log.Fatalf("could not get certfile origin: %v", err)
			}
			if err := h.x.Get(&keyfile, `SELECT s FROM config WHERE key = $1`, cloudflareOrigin+"-key"); err != nil {
				log.Fatalf("could not get keyfile origin: %v", err)
			}
			cert, err := tls.X509KeyPair(certfile, keyfile)
			if err != nil {
				log.Fatalf("cert: %v", err)
			}
			tlsConfig = &tls.Config{
				Certificates: []tls.Certificate{cert},
			}
		} else {
			m := autocert.Manager{
				Prompt:     autocert.AcceptTOS,
				HostPolicy: autocert.HostWhitelist(*flagAutocert),
				Cache:      dbCache{db},
				Client: &acme.Client{
					DirectoryURL: *flagAcmedir,
				},
			}
			tlsConfig = &tls.Config{GetCertificate: m.GetCertificate}
		}
		s := &http.Server{
			Addr:      ":https",
			TLSConfig: tlsConfig,
		}
		log.Fatal(s.ListenAndServeTLS("", ""))
	} else {
		fmt.Println("HTTP listen on addr:", *flagAddr)
		log.Fatal(http.ListenAndServe(*flagAddr, nil))
	}
}

var (
	enableDBCache = map[string]bool{
		"/api/get-build-winrates": true,
		"/api/get-compare-hero":   true,
		"/api/get-hero-data":      true,
		"/api/get-winrates":       true,
	}
	enableMemCache = map[string]bool{
		"/api/init": true,
	}
)

func resultToBytes(res interface{}) (data, gzipped []byte, err error) {
	data, err = json.Marshal(res)
	if err != nil {
		return nil, nil, errors.Wrap(err, "json marshal")
	}
	var gz bytes.Buffer
	gzw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	if _, err := gzw.Write(data); err != nil {
		return nil, nil, errors.Wrap(err, "gzip")
	}
	if err := gzw.Close(); err != nil {
		return nil, nil, errors.Wrap(err, "gzip close")
	}
	return data, gz.Bytes(), nil
}

func (h *hotsContext) CheckCache(ctx context.Context, start time.Time, w http.ResponseWriter, r *http.Request, path, url string) (done bool) {
	if !enableMemCache[path] && !enableDBCache[path] {
		return false
	}
	h.mu.RLock()
	c, ok := h.mu.cache[url]
	h.mu.RUnlock()
	if ok && c.until > start.Unix() {
		writeDataGzip(w, r, c.data, c.gzip)
		return true
	}
	if !enableDBCache[path] {
		return false
	}
	var data, gz []byte
	if err := h.db.QueryRowContext(ctx,
		"SELECT data, gzip FROM cache WHERE id = $1",
		url,
	).Scan(&data, &gz); err == nil {
		writeDataGzip(w, r, data, gz)
		h.mu.Lock()
		h.mu.cache[url] = cache{
			until: start.Add(h.cacheTime).Unix(),
			data:  data,
			gzip:  gz,
		}
		h.mu.Unlock()
		// Don't block user return on db writes.
		go func() {
			if err := retry(func() error {
				_, err := h.db.Exec(`UPDATE cache SET last_hit = $1 WHERE id = $2`, start, url)
				return err
			}); err != nil {
				log.Printf("couldn't update cache last_hit: %s: %s", url, err)
			}
		}()
		return true
	}
	return false
}

func (h *hotsContext) WriteCache(path, url string, start time.Time, data, gzip []byte) {
	if !enableMemCache[path] && !enableDBCache[path] {
		return
	}
	until := start.Add(h.cacheTime)
	h.mu.Lock()
	h.mu.cache[url] = cache{
		until: until.Unix(),
		data:  data,
		gzip:  gzip,
	}
	h.mu.Unlock()
	if !enableDBCache[path] {
		return
	}
	if err := retry(func() error {
		_, err := h.db.Exec("UPSERT INTO cache (id, data, gzip, last_hit, until) VALUES ($1, $2, $3, $4, NULL)",
			url,
			data,
			gzip,
			start,
		)
		return err
	}); err != nil {
		log.Printf("update cache table: %s: %v", url, err)
	}
}

func writeDataGzip(w http.ResponseWriter, r *http.Request, data, gzip []byte) {
	w.Header().Add("Content-Type", "application/json")
	w.Header().Add("Cache-Control", "max-age=3600")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Add("Content-Encoding", "gzip")
		w.Write(gzip)
	} else {
		w.Write(data)
	}
}

const autocertPrefix = "autocert-"

type dbCache struct {
	*sql.DB
}

func (db dbCache) Get(ctx context.Context, key string) ([]byte, error) {
	var data []byte
	if err := db.QueryRowContext(ctx, "SELECT s FROM config WHERE key = $1", autocertPrefix+key).Scan(&data); err == sql.ErrNoRows {
		return nil, autocert.ErrCacheMiss
	} else if err != nil {
		return nil, err
	}
	return data, nil
}

func (db dbCache) Put(ctx context.Context, key string, data []byte) error {
	_, err := db.ExecContext(ctx, "UPSERT INTO config (key, s) VALUES ($1, $2)", autocertPrefix+key, data)
	return err
}

func (db dbCache) Delete(ctx context.Context, key string) error {
	_, err := db.ExecContext(ctx, "DELETE FROM config WHERE key = $1", autocertPrefix+key)
	return err
}

type hotsContext struct {
	db        *sql.DB
	x         *sqlx.DB
	cacheTime time.Duration

	mu struct {
		sync.RWMutex
		cache map[string]cache
		init  initData
	}
}

type cache struct {
	until int64
	data  []byte
	gzip  []byte
}

type initData struct {
	Modes      map[Mode]string
	Builds     []Build
	Maps       []string
	Heroes     []Hero
	BuildStats map[string]map[Mode]Stats
	config     *groupConfig
	lookups    map[string]func(string) string
}

func (i initData) list(name, s string) []string {
	lookup := i.lookups[name]
	var res []string
	for _, v := range strings.Split(s[1:len(s)-1], ",") {
		res = append(res, lookup(v))
	}
	return res
}

func (h *hotsContext) Init(ctx context.Context, _ *http.Request) (interface{}, error) {
	return h.getInit(), nil
}

func (h *hotsContext) updateInit(ctx context.Context) error {
	var maps []byte
	if err := h.x.GetContext(ctx, &maps, "SELECT s FROM config WHERE key = $1", cacheConfig); err != nil {
		return errors.Wrap(err, "get config")
	}
	var c groupConfig
	if err := json.Unmarshal(maps, &c); err != nil {
		return err
	}
	c.readonly = true
	bs := make(map[string]map[Mode]Stats)
	/*
		{
			rows, err := h.db.QueryContext(ctx, "SELECT * FROM skillstats")
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var build string
				var mode Mode
				var data []byte
				if err := rows.Scan(&build, &mode, &data); err != nil {
					return err
				}
				var s Stats
				if err := json.Unmarshal(data, &s); err != nil {
					return err
				}
				if _, ok := bs[build]; !ok {
					bs[build] = make(map[Mode]Stats)
				}
				bs[build][mode] = s
			}
			if err := rows.Err(); err != nil {
				return err
			}
		}
	*/
	h.mu.Lock()
	h.mu.init = initData{
		Modes:      modeNames,
		Heroes:     heroData,
		BuildStats: bs,
		config:     &c,
		lookups:    make(map[string]func(string) string),
	}
	for m := range c.Map["map"] {
		h.mu.init.Maps = append(h.mu.init.Maps, m)
	}
	sort.Strings(h.mu.init.Maps)
	for n, b := range c.Builds {
		h.mu.init.Builds = append(h.mu.init.Builds, Build{
			ID:     n,
			Start:  b.Start,
			Finish: b.End,
		})
	}
	sort.Slice(h.mu.init.Builds, func(i, j int) bool {
		return h.mu.init.Builds[i].ID > h.mu.init.Builds[j].ID
	})
	for group, m := range c.Map {
		lookup := make(map[string]string)
		for k, v := range m {
			lookup[v] = k
		}
		h.mu.init.lookups[group] = func(name string) string {
			return lookup[name]
		}
	}
	h.mu.Unlock()
	return nil
}

func (h *hotsContext) ClearCache(_ http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	h.mu.cache = make(map[string]cache)
	if err := retry(func() error {
		_, err := h.db.Exec("DELETE FROM cache")
		return err
	}); err != nil {
		log.Println(err)
	}
	h.mu.Unlock()
	if err := h.updateInit(context.Background()); err != nil {
		log.Println(err)
	}
}

// txn executes a transaction. If the database returns a retryable error,
// fn is re-invoked. fn should not call Commit or Rollback.
func (h *hotsContext) txn(ctx context.Context, fn func(txn *sqlx.Tx) error) error {
	return retry(func() error {
		txn, err := h.x.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		err = fn(txn)
		if err == nil {
			return txn.Commit()
		}
		txn.Rollback()
		return err
	})
}

// retry executes fn, but retries it if fn returns a retryable postgres error.
func retry(fn func() error) error {
	for count := 0; count < 10; count++ {
		err := fn()
		if err == nil {
			return nil
		}

		if retryable(err) {
			continue
		}
		return err
	}
	return errors.New("retry limit exhausted")
}

func retryable(err error) bool {
	err = errors.Cause(err)

	pqErr, ok := err.(*pq.Error)
	if ok && pqErr.Code == "40001" {
		return true
	}

	if strings.Contains(err.Error(), "connection reset by peer") {
		return true
	}

	return false
}

func (h *hotsContext) GetBuildWinrates(ctx context.Context, r *http.Request) (interface{}, error) {
	args := map[string]string{
		"build":     r.FormValue("build"),
		"hero":      r.FormValue("hero"),
		"herolevel": r.FormValue("herolevel"),
		"map":       r.FormValue("map"),
		"mode":      r.FormValue("mode"),
	}
	init := h.getInit()
	var res struct {
		Current       map[int]map[string]Total
		Previous      map[int]map[string]Total
		PopularBuilds []build
		WinningBuilds []build
		Talents       map[string]talentText
	}
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		var err error
		res.Current, res.PopularBuilds, res.WinningBuilds, err = h.getBuildWinrates(ctx, init, args)
		return errors.Wrap(err, "getBuildWinrates")
	})
	g.Go(func() error {
		if prevBuild, ok := h.getBuildBefore(init, args["build"]); ok {
			argsPrev := make(map[string]string, len(args))
			for k, v := range args {
				argsPrev[k] = v
			}
			var err error
			argsPrev["build"] = prevBuild
			res.Previous, _, _, err = h.getBuildWinrates(ctx, init, argsPrev)
			return errors.Wrapf(err, "fetch previous build: %v", prevBuild)
		}
		return nil
	})
	err := g.Wait()
	m := make(map[string]talentText)
	for _, talents := range res.Current {
		for id := range talents {
			m[id] = talentData[id]
		}
	}
	res.Talents = m
	return res, err
}

type build struct {
	Build   []string
	Total   int
	Winrate float64
}

func (h *hotsContext) getBuildWinrates(ctx context.Context, init initData, args map[string]string) (map[int]map[string]Total, []build, []build, error) {
	if args["build"] == "" {
		return nil, nil, nil, errors.New("build required")
	}
	if args["hero"] == "" {
		return nil, nil, nil, errors.New("hero required")
	}

	groups := []string{"talents", "winner"}
	var wheres []string
	var params []interface{}
	for _, key := range []string{"build", "hero", "map", "mode"} {
		v := args[key]
		if v == "" {
			continue
		}
		if m, ok := init.config.Map[key]; ok {
			v = m[v]
			if v == "" {
				return nil, nil, nil, errors.Errorf("unrecognized %s: %s", key, args[key])
			}
		}
		wheres = append(wheres, fmt.Sprintf("%s = $%d", key, len(params)+1))
		groups = append(groups, key)
		params = append(params, v)
	}
	hl := args["herolevel"]
	if hl == "" {
		hl = defaultHerolevel
	}
	wheres = append(wheres, fmt.Sprintf("hero_level >= $%d", len(params)+1))
	params = append(params, hl)

	buf := bytes.NewBufferString(`SELECT COUNT(*) count, winner, talents`)
	buf.WriteString(" FROM players")
	wheres = append(wheres, "array_length(talents, 1) > 0")
	if len(wheres) > 0 {
		fmt.Fprintf(buf, " WHERE %s", strings.Join(wheres, " AND "))
	}
	buf.WriteString(" GROUP BY ")
	buf.WriteString(strings.Join(groups, ", "))
	query := buf.String()

	tally := make(map[int]map[string]Total)
	for i := 1; i <= 7; i++ {
		tally[i] = make(map[string]Total)
	}

	var winrates []struct {
		Count   int
		Talents string
		Winner  bool
	}
	if err := h.x.Select(&winrates, query, params...); err != nil {
		return nil, nil, nil, errors.Wrap(err, "select")
	}
	total := make(map[string]struct {
		Total int
		Won   int
	})
	for _, wr := range winrates {
		talents := wr.Talents[1 : len(wr.Talents)-1]
		t := total[talents]
		t.Total += wr.Count
		if wr.Winner {
			t.Won += wr.Count
		}
		total[talents] = t
		for tier, talent := range strings.Split(talents, ",") {
			tier += 1
			talent := string(init.lookups["talent"](string(talent)))
			t := tally[tier][talent]
			if wr.Winner {
				t.Wins += wr.Count
			} else {
				t.Losses += wr.Count
			}
			tally[tier][talent] = t
		}
	}
	var builds []build
	for n, t := range total {
		talents := strings.Split(n, ",")
		var talentNames []string
		for _, talent := range talents {
			talentNames = append(talentNames, init.lookups["talent"](talent))
		}
		b := build{
			Build: talentNames,
			Total: t.Total,
		}
		if t.Won > popularGameLimit {
			b.Winrate = float64(t.Won) / float64(t.Total)
		}
		builds = append(builds, b)
	}
	n := 5
	if n > len(builds) {
		n = len(builds)
	}
	var popularBuilds, winningBuilds []build
	sort.Slice(builds, func(i, j int) bool {
		return builds[i].Total > builds[j].Total
	})
	for i := 0; i < n; i++ {
		b := builds[i]
		if b.Total > popularGameLimit {
			popularBuilds = append(popularBuilds, b)
		}
	}
	sort.Slice(builds, func(i, j int) bool {
		return builds[i].Winrate > builds[j].Winrate
	})
	for i := 0; i < n; i++ {
		b := builds[i]
		if b.Winrate > 0 {
			winningBuilds = append(winningBuilds, b)
		}
	}
	return tally, popularBuilds, winningBuilds, nil
}

func (h *hotsContext) GetPlayerName(ctx context.Context, r *http.Request) (interface{}, error) {
	name := r.FormValue("name")
	if name == "" {
		return nil, errors.New("no name parameter")
	}
	type entry struct {
		ID   int64
		Name string
	}
	var res []entry
	var last string
	for i := 0; i < 10; i++ {
		var e entry
		err := h.x.GetContext(ctx, &e, `
			SELECT blizzid id , battletag "name" FROM players
			WHERE battletag LIKE $1
			AND battletag > $2
			LIMIT 1
		`, name+"%", last)
		if err == sql.ErrNoRows {
			break
		} else if err != nil {
			return nil, err
		}
		last = e.Name
		res = append(res, e)
	}
	return res, nil
}

func (h *hotsContext) GetPlayerData(ctx context.Context, r *http.Request) (interface{}, error) {
	id := r.FormValue("id")
	init := h.getInit()
	if id == "" {
		return nil, errors.New("no id parameter")
	}
	var res struct {
		Battletag string
		Skills    []struct {
			Build string
			Mode  Mode
			Skill int
		}
		Games []struct {
			Game      int
			Hero      string
			HeroLevel int    `db:"hero_level"`
			Date      string `db:"time"`
			Build     string
			Winner    bool
			Length    int
			Map       string
			Mode      Mode
			Skill     *int
		}
	}
	/*
		if err := h.x.SelectContext(ctx, &res.Skills, `
			SELECT build, mode, skill FROM playerskills
			WHERE blizzid = $1
			`, id); err != nil {
			return nil, err
		}
		sort.Slice(res.Skills, func(i, j int) bool {
			a := res.Skills[j]
			b := res.Skills[i]
			if a.Build != b.Build {
				return a.Build < b.Build
			}
			return a.Mode < b.Mode
		})
	*/

	if err := h.x.GetContext(ctx, &res.Battletag, `
			SELECT battletag
			FROM players
			WHERE blizzid = $1
			ORDER BY time DESC
			LIMIT 1
			`, id); err != nil {
		return nil, err
	}

	if err := h.x.SelectContext(ctx, &res.Games, `
			SELECT game, hero, hero_level, build, winner, length, map, mode, time
			FROM players
			WHERE blizzid = $1
			ORDER BY time DESC
			LIMIT 1000
			`, id); err != nil {
		return nil, err
	}
	for i, g := range res.Games {
		g.Hero = init.lookups["hero"](g.Hero)
		g.Map = init.lookups["map"](g.Map)
		g.Build = init.lookups["build"](g.Build)
		res.Games[i] = g
	}

	return res, nil
}

func (h *hotsContext) GetGameData(ctx context.Context, r *http.Request) (interface{}, error) {
	id := r.FormValue("id")
	init := h.getInit()
	if id == "" {
		return nil, errors.New("no id parameter")
	}
	res := struct {
		Game struct {
			Mode    Mode
			Date    string `db:"time"`
			Map     string
			Length  int
			Build   string
			Bans    string `json:"-"`
			BanList []string
		}
		Players []*struct {
			Hero       string
			HeroLevel  int `db:"hero_level"`
			Winner     bool
			Blizzid    int
			Battletag  string
			Talents    string `json:"-"`
			TalentList []string
		}
		Talents map[string]talentText
	}{
		Talents: make(map[string]talentText),
	}

	if err := h.x.GetContext(ctx, &res.Game, `
		SELECT mode, time, map, length, build, bans
		FROM games
		WHERE id = $1
		`, id); err != nil {
		return nil, err
	}

	if err := h.x.SelectContext(ctx, &res.Players, `
		SELECT hero, hero_level, winner, blizzid, battletag, talents
		FROM players
		WHERE game = $1
		`, id); err != nil {
		return nil, err
	}
	res.Game.BanList = init.list("hero", res.Game.Bans)
	res.Game.Map = init.lookups["map"](res.Game.Map)
	res.Game.Build = init.lookups["build"](res.Game.Build)
	for _, p := range res.Players {
		p.Hero = init.lookups["hero"](p.Hero)
		p.TalentList = init.list("talent", p.Talents)
		for _, t := range p.TalentList {
			res.Talents[t] = talentData[t]
		}
	}

	return res, nil
}

type heroRelativeData struct {
	Base    map[string]Total
	Lengths map[string]Total
	Levels  map[string]Total
	Maps    map[string]Total
	Modes   map[string]Total
}

func (h *hotsContext) GetHero(ctx context.Context, r *http.Request) (interface{}, error) {
	init := h.getInit()
	build := init.config.build(r.FormValue("build"))
	hero := init.config.hero(r.FormValue("hero"))
	var res struct {
		Current  heroRelativeData
		Previous heroRelativeData
	}
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		var err error
		res.Current, err = h.getHero(ctx, init, build, hero)
		return errors.Wrap(err, "getHero current build")
	})
	g.Go(func() error {
		if prevBuild, ok := h.getBuildBefore(init, r.FormValue("build")); ok {
			var err error
			res.Previous, err = h.getHero(ctx, init, init.config.build(prevBuild), hero)
			return errors.Wrap(err, "getHero previous build")
		}
		return nil
	})
	err := g.Wait()
	return res, err
}

func (h *hotsContext) getHero(ctx context.Context, init initData, build, hero string) (heroRelativeData, error) {
	params := []interface{}{
		build,
		hero,
	}
	var res heroRelativeData
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		var err error
		res.Base, err = h.countWins(ctx, nil, `
			SELECT COUNT(*) count, winner, '' counter
			FROM players
			WHERE build = $1 AND hero = $2
			GROUP BY winner
		`, params)
		if len(res.Base) == 0 {
			res.Base[""] = Total{}
		}
		return errors.Wrap(err, "base")
	})
	g.Go(func() error {
		var err error
		res.Maps, err = h.countWins(ctx, init.lookups["map"], `
			SELECT COUNT(*) count, winner, map counter
			FROM players
			WHERE build = $1 AND hero = $2
			GROUP BY winner, map
		`, params)
		return errors.Wrap(err, "maps")
	})
	g.Go(func() error {
		var err error
		res.Modes, err = h.countWins(ctx, nil, `
			SELECT COUNT(*) count, winner, mode counter
			FROM players
			WHERE build = $1 AND hero = $2
			GROUP BY winner, mode
		`, params)
		return errors.Wrap(err, "modes")
	})
	g.Go(func() error {
		var err error
		// Group hero levels  minute blocks.
		res.Levels, err = h.countWins(ctx, nil, `
			SELECT count(*) count, winner, counter * 5 as counter
			FROM (
				SELECT winner, round(hero_level / 5) counter
				FROM players
				WHERE build = $1 AND hero = $2
			)
			GROUP BY winner, counter
		`, params)
		return errors.Wrap(err, "hero level")
	})
	g.Go(func() error {
		var err error
		// Group game lengths in 5 minute blocks.
		res.Lengths, err = h.countWins(ctx, nil, `
			SELECT count(*) count, winner, counter * 60 * 5 as counter
			FROM (
				SELECT winner, round(length / 60 / 5) as counter
				FROM players
				WHERE build = $1 AND hero = $2
			)
			GROUP BY winner, counter
		`, params)
		return errors.Wrap(err, "length")
	})
	err := g.Wait()
	return res, err
}

func (h *hotsContext) countWins(ctx context.Context, nameFn func(string) string, query string, params []interface{}) (map[string]Total, error) {
	tally := make(map[string]Total)
	var winrates []struct {
		Counter string
		Count   int
		Winner  bool
	}
	if err := h.x.Select(&winrates, query, params...); err != nil {
		return nil, errors.Wrap(err, "select wins")
	}
	for _, wr := range winrates {
		n := wr.Counter
		if nameFn != nil {
			n = nameFn(n)
		}
		t := tally[n]
		if wr.Winner {
			t.Wins += wr.Count
		} else {
			t.Losses += wr.Count
		}
		tally[n] = t
	}
	return tally, nil
}

func (h *hotsContext) getInit() initData {
	h.mu.RLock()
	init := h.mu.init
	h.mu.RUnlock()
	return init
}

func (h *hotsContext) GetWinrates(ctx context.Context, r *http.Request) (interface{}, error) {
	args := map[string]string{
		"build":      r.FormValue("build"),
		"herolevel":  r.FormValue("herolevel"),
		"map":        r.FormValue("map"),
		"mode":       r.FormValue("mode"),
		"skill_low":  r.FormValue("skill_low"),
		"skill_high": r.FormValue("skill_high"),
	}
	init := h.getInit()
	var res struct {
		Current  map[string]Total
		Previous map[string]Total
	}
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		var err error
		res.Current, err = h.getWinrates(ctx, init, args)
		return errors.Wrap(err, "getWinrates current build")
	})
	g.Go(func() error {
		if prevBuild, ok := h.getBuildBefore(init, args["build"]); ok {
			argsPrev := make(map[string]string, len(args))
			for k, v := range args {
				argsPrev[k] = v
			}
			var err error
			argsPrev["build"] = prevBuild
			res.Previous, err = h.getWinrates(ctx, init, argsPrev)
			return errors.Wrap(err, "getWinrates previous build")
		}
		return nil
	})
	err := g.Wait()
	return res, err
}

const defaultHerolevel = "5"

func (h *hotsContext) getWinrates(ctx context.Context, init initData, args map[string]string) (map[string]Total, error) {
	if args["build"] == "" {
		return nil, errors.New("build required")
	}

	groups := []string{"hero", "winner"}
	var wheres []string
	var params []interface{}
	for _, key := range []string{"build", "map", "mode"} {
		v := args[key]
		if v == "" {
			continue
		}
		if m, ok := init.config.Map[key]; ok {
			v = m[v]
			if v == "" {
				return nil, errors.Errorf("unrecognized %s: %s", key, args[key])
			}
		}
		wheres = append(wheres, fmt.Sprintf("%s = $%d", key, len(params)+1))
		params = append(params, v)
	}
	hl := args["herolevel"]
	if hl == "" {
		hl = defaultHerolevel
	}
	wheres = append(wheres, fmt.Sprintf("hero_level >= $%d", len(params)+1))
	params = append(params, hl)
	if m, sl, sh := args["mode"], args["skill_low"], args["skill_high"]; m != "" && (sl != "" || sh != "") {
		i, err := strconv.Atoi(m)
		if err != nil {
			return nil, err
		}
		modes, ok := init.BuildStats[args["build"]]
		if !ok {
			return nil, errors.Errorf("unknown build: %s", args["build"])
		}
		quantiles := modes[Mode(i)].Quantile
		if sl != "" {
			wheres = append(wheres, fmt.Sprintf("skill >= $%d", len(params)+1))
			i, err = strconv.Atoi(sl)
			if err != nil {
				return nil, err
			}
			params = append(params, quantiles[i])
		}
		if sh != "" {
			wheres = append(wheres, fmt.Sprintf("skill <= $%d", len(params)+1))
			i, err = strconv.Atoi(sh)
			if err != nil {
				return nil, err
			}
			params = append(params, quantiles[i])
		}
	}

	buf := bytes.NewBufferString("SELECT COUNT(*) count, hero counter, winner FROM players")
	if len(wheres) > 0 {
		fmt.Fprintf(buf, " WHERE %s", strings.Join(wheres, " AND "))
	}
	buf.WriteString(" GROUP BY ")
	buf.WriteString(strings.Join(groups, ", "))

	return h.countWins(ctx, init.lookups["hero"], buf.String(), params)
}

type Total struct {
	Wins, Losses int
}

func (h *hotsContext) GetCompareHero(ctx context.Context, r *http.Request) (interface{}, error) {
	init := h.getInit()
	args := map[string]string{
		"build":     init.config.build(r.FormValue("build")),
		"hero":      init.config.hero(r.FormValue("hero")),
		"herolevel": r.FormValue("herolevel"),
		"map":       init.config.gamemap(r.FormValue("map")),
		"mode":      r.FormValue("mode"),
	}
	if args["build"] == "" {
		return nil, errors.New("build required")
	}
	if args["hero"] == "" {
		return nil, errors.New("hero required")
	}

	var wheres []string
	var params []interface{}
	for _, key := range []string{"build", "hero", "map", "mode"} {
		v := args[key]
		if v == "" {
			continue
		}
		wheres = append(wheres, fmt.Sprintf("%s = $%d", key, len(params)+1))
		params = append(params, v)
	}
	hl := args["herolevel"]
	if hl == "" {
		hl = defaultHerolevel
	}
	wheres = append(wheres, fmt.Sprintf("hero_level >= $%d", len(params)+1))
	params = append(params, hl)
	var games []struct {
		Game   int64
		Team   int
		Winner bool
	}
	if err := h.x.SelectContext(ctx, &games, fmt.Sprintf(`
		SELECT game, team, winner
		FROM players
		WHERE %s
		`, strings.Join(wheres, " AND "),
	), params...); err != nil {
		return nil, err
	}
	var total Total
	team0 := make([]interface{}, 0, len(games))
	team1 := make([]interface{}, 0, len(games))
	for _, g := range games {
		if g.Team == 0 {
			team0 = append(team0, g.Game)
		} else {
			team1 = append(team1, g.Game)
		}
		if g.Winner {
			total.Wins++
		} else {
			total.Losses++
		}
	}
	sameTeam := make(map[string]Total)
	otherTeam := make(map[string]Total)
	getGames := func(team int, ids []interface{}) error {
		for len(ids) > 0 {
			limit := 1000
			if limit > len(ids) {
				limit = len(ids)
			}
			params := append([]interface{}{team, args["hero"]}, ids[:limit]...)
			query := fmt.Sprintf(`
					SELECT hero, team = $1 as sameteam, winner, count(*) as count
					FROM players
					WHERE
						game IN %s
						AND hero != $2
					GROUP BY hero, team, winner
					`, makeValues(limit, 3),
			)
			var res []struct {
				Hero     string
				Sameteam bool
				Winner   bool
				Count    int
			}
			if err := h.x.SelectContext(ctx, &res, query, params...); err != nil {
				return errors.Wrap(err, "select")
			}
			for _, r := range res {
				team := sameTeam
				if !r.Sameteam {
					team = otherTeam
				}
				hero := init.lookups["hero"](r.Hero)
				t := team[hero]
				if r.Winner == r.Sameteam {
					t.Wins += r.Count
				} else {
					t.Losses += r.Count
				}
				team[hero] = t
			}
			ids = ids[limit:]
		}
		return nil
	}
	if err := getGames(0, team0); err != nil {
		return nil, err
	}
	if err := getGames(1, team1); err != nil {
		return nil, err
	}
	return struct {
		SameTeam  map[string]Total
		OtherTeam map[string]Total
		Total     Total
	}{
		SameTeam:  sameTeam,
		OtherTeam: otherTeam,
		Total:     total,
	}, nil
}

func (h *hotsContext) getBuildBefore(init initData, id string) (build string, ok bool) {
	for i, b := range init.Builds {
		if b.ID == id {
			if len(init.Builds) == i+1 {
				return "", false
			}
			return init.Builds[i+1].ID, true
		}
	}
	return "", false
}

var capsRE = regexp.MustCompile(`[A-Z][a-z]+`)
var font *truetype.Font

func init() {
	var err error
	font, err = freetype.ParseFont(goregular.TTF)
	if err != nil {
		panic(err)
	}
}

func makeTalentImg(w http.ResponseWriter, r *http.Request) {
	idx := strings.LastIndexByte(r.URL.Path, '/')
	name := r.URL.Path[idx+1:]
	words := capsRE.FindAllStringSubmatch(name, 4)
	i := image.NewRGBA(image.Rect(0, 0, 40, 40))
	draw.Draw(i, i.Bounds(), &image.Uniform{image.White}, image.ZP, draw.Src)

	const size = 10
	c := freetype.NewContext()
	c.SetFont(font)
	c.SetFontSize(size)
	c.SetClip(i.Bounds())
	c.SetDst(i)
	c.SetSrc(image.Black)

	for i, word := range words {
		if _, err := c.DrawString(word[0], freetype.Pt(1, (i+1)*size-2)); err != nil {
			log.Printf("%s: %+v", r.URL.Path, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Add("Cache-Control", "max-age=3600")
	if err := png.Encode(w, i); err != nil {
		log.Printf("%s: %+v", r.URL.Path, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}
