package main

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/pkg/errors"
)

func (h *hotsContext) cron() error {
	if err := h.updateInit(context.Background()); err != nil {
		return errors.Wrap(err, "update init")
	}
	if err := retry(func() error {
		_, err := h.db.Exec(`DELETE FROM cache WHERE last_hit < (now() - '48h'::interval)`)
		return err
	}); err != nil {
		return errors.Wrap(err, "empty cache")
	}
	var urls []string
	if err := retry(func() error {
		rows, err := h.db.Query(`SELECT id FROM cache WHERE until < now() OR until IS NULL`)
		if err != nil {
			return err
		}
		defer rows.Close()
		urls = urls[:0]
		for rows.Next() {
			var u string
			if err := rows.Scan(&u); err != nil {
				return err
			}
			urls = append(urls, u)
		}
		return nil
	}); err != nil {
		return errors.Wrap(err, "fetch urls")
	}
	for _, u := range urls {
		url, err := url.Parse(u)
		if err != nil {
			return err
		}
		req := &http.Request{URL: url}
		ctx, _ := context.WithTimeout(context.Background(), time.Minute)
		var res interface{}
		switch url.Path {
		case "/api/get-winrates":
			var err error
			res, err = h.GetWinrates(ctx, req)
			if err != nil {
				return errors.Wrap(err, "get winrates")
			}
		default:
			return errors.Errorf("unknown path: %s", u)
		}
		data, gzip, err := resultToBytes(res)
		if err != nil {
			return err
		}
		if err := retry(func() error {
			_, err := h.db.Exec(`UPDATE cache SET data = $1, gzip = $2, until = now() WHERE id = $3`,
				data, gzip, u,
			)
			return err
		}); err != nil {
			return errors.Wrap(err, "empty cache")
		}
		log.Printf("recached %s", url)
	}
	if err := doGenerateHeroes(h.db); err != nil {
		return err
	}
	return nil
}